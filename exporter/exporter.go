package exporter

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"github.com/aquasecurity/libbpfgo"
	"github.com/cloudflare/ebpf_exporter/config"
	"github.com/cloudflare/ebpf_exporter/decoder"
	"github.com/cloudflare/ebpf_exporter/util"
	"github.com/prometheus/client_golang/prometheus"
)

// Namespace to use for all metrics
const prometheusNamespace = "ebpf_exporter"

// Exporter is a ebpf_exporter instance implementing prometheus.Collector
type Exporter struct {
	config                   config.Config
	modules                  map[string]*libbpfgo.Module
	perfEventArrayCollectors []*PerfEventArraySink
	kaddrs                   map[string]uint64
	enabledProgramsDesc      *prometheus.Desc
	programInfoDesc          *prometheus.Desc
	programTags              map[string]map[string]string
	descs                    map[string]map[string]*prometheus.Desc
	decoders                 *decoder.Set
}

// New creates a new exporter with the provided config
func New(cfg config.Config) (*Exporter, error) {
	err := config.ValidateConfig(&cfg)
	if err != nil {
		return nil, fmt.Errorf("error validating config: %s", err)
	}

	enabledProgramsDesc := prometheus.NewDesc(
		prometheus.BuildFQName(prometheusNamespace, "", "enabled_programs"),
		"The set of enabled programs",
		[]string{"name"},
		nil,
	)

	programInfoDesc := prometheus.NewDesc(
		prometheus.BuildFQName(prometheusNamespace, "", "ebpf_programs"),
		"Info about ebpf programs",
		[]string{"program", "function", "tag"},
		nil,
	)

	return &Exporter{
		config:              cfg,
		modules:             map[string]*libbpfgo.Module{},
		kaddrs:              map[string]uint64{},
		enabledProgramsDesc: enabledProgramsDesc,
		programInfoDesc:     programInfoDesc,
		programTags:         map[string]map[string]string{},
		descs:               map[string]map[string]*prometheus.Desc{},
		decoders:            decoder.NewSet(),
	}, nil
}

// Attach injects eBPF into kernel and attaches necessary kprobes
func (e *Exporter) Attach(configPath string) error {
	err := registerHandlers()
	if err != nil {
		return fmt.Errorf("error registering libbpf handlers: %v", err)
	}

	for _, program := range e.config.Programs {
		if _, ok := e.modules[program.Name]; ok {
			return fmt.Errorf("multiple programs with name %q", program.Name)
		}

		bpfProgPath := filepath.Join(configPath, fmt.Sprintf("%s.bpf.o", program.Name))
		module, err := libbpfgo.NewModuleFromFile(bpfProgPath)
		if err != nil {
			return fmt.Errorf("error creating module from %q for program %q: %v", bpfProgPath, program.Name, err)
		}

		if len(program.Kaddrs) > 0 {
			err = e.passKaddrs(module, program)
			if err != nil {
				return fmt.Errorf("error passing kaddrs to program %q: %v", program.Name, err)
			}
		}

		err = module.BPFLoadObject()
		if err != nil {
			return fmt.Errorf("error loading bpf object from %q for program %q: %v", bpfProgPath, program.Name, err)
		}

		tags, err := attachModule(module, program)
		if err != nil {
			return fmt.Errorf("failed to attach to program %q: %s", program.Name, err)
		}

		e.programTags[program.Name] = tags
		e.modules[program.Name] = module
	}

	return nil
}

func (e *Exporter) passKaddrs(module *libbpfgo.Module, program config.Program) error {
	if len(e.kaddrs) == 0 {
		if err := e.populateKaddrs(); err != nil {
			return fmt.Errorf("error populating kaddrs: %v", err)
		}
	}

	for _, kaddr := range program.Kaddrs {
		if addr, ok := e.kaddrs[kaddr]; !ok {
			return fmt.Errorf("error finding kaddr for %q", kaddr)
		} else {
			name := fmt.Sprintf("kaddr_%s", kaddr)
			if err := module.InitGlobalVariable(name, uint64(addr)); err != nil {
				return fmt.Errorf("error setting kaddr value for %q (const volatile %q) to 0x%x: %v", kaddr, name, addr, err)
			}
		}
	}

	return nil
}

// populateKaddrs populates cache of ksym -> kaddr mappings
func (e Exporter) populateKaddrs() error {
	fd, err := os.Open("/proc/kallsyms")
	if err != nil {
		return err
	}

	defer fd.Close()

	s := bufio.NewScanner(fd)
	for s.Scan() {
		parts := strings.Split(s.Text(), " ")
		if len(parts) != 3 {
			continue
		}

		addr, err := strconv.ParseUint(parts[0], 16, 64)
		if err != nil {
			return fmt.Errorf("error parsing addr %q from line %q: %s", parts[0], s.Text(), err)
		}

		e.kaddrs[parts[2]] = addr
	}

	return s.Err()
}

// Describe satisfies prometheus.Collector interface by sending descriptions
// for all metrics the exporter can possibly report
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	addDescs := func(programName string, name string, help string, labels []config.Label) {
		if _, ok := e.descs[programName][name]; !ok {
			labelNames := []string{}

			for _, label := range labels {
				labelNames = append(labelNames, label.Name)
			}

			e.descs[programName][name] = prometheus.NewDesc(prometheus.BuildFQName(prometheusNamespace, "", name), help, labelNames, nil)
		}

		ch <- e.descs[programName][name]
	}

	ch <- e.enabledProgramsDesc
	ch <- e.programInfoDesc

	for _, program := range e.config.Programs {
		if _, ok := e.descs[program.Name]; !ok {
			e.descs[program.Name] = map[string]*prometheus.Desc{}
		}

		for _, counter := range program.Metrics.Counters {
			if counter.PerfEventArray {
				perfSink := NewPerfEventArraySink(e.decoders, e.modules[program.Name], counter)
				e.perfEventArrayCollectors = append(e.perfEventArrayCollectors, perfSink)
			}

			addDescs(program.Name, counter.Name, counter.Help, counter.Labels)
		}

		for _, histogram := range program.Metrics.Histograms {
			addDescs(program.Name, histogram.Name, histogram.Help, histogram.Labels[0:len(histogram.Labels)-1])
		}
	}
}

// Collect satisfies prometheus.Collector interface and sends all metrics
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	for _, program := range e.config.Programs {
		ch <- prometheus.MustNewConstMetric(e.enabledProgramsDesc, prometheus.GaugeValue, 1, program.Name)
	}

	for program, tags := range e.programTags {
		for function, tag := range tags {
			ch <- prometheus.MustNewConstMetric(e.programInfoDesc, prometheus.GaugeValue, 1, program, function, tag)
		}
	}

	for _, perfEventArrayCollector := range e.perfEventArrayCollectors {
		perfEventArrayCollector.Collect(ch)
	}

	e.collectCounters(ch)
	e.collectHistograms(ch)
}

// collectCounters sends all known counters to prometheus
func (e *Exporter) collectCounters(ch chan<- prometheus.Metric) {
	for _, program := range e.config.Programs {
		for _, counter := range program.Metrics.Counters {
			if counter.PerfEventArray {
				continue
			}

			mapValues, err := e.mapValues(e.modules[program.Name], counter.Name, counter.Labels)
			if err != nil {
				log.Printf("Error getting map %q values for metric %q of program %q: %s", counter.Name, counter.Name, program.Name, err)
				continue
			}

			desc := e.descs[program.Name][counter.Name]

			for _, metricValue := range mapValues {
				ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, metricValue.value, metricValue.labels...)
			}
		}
	}
}

// collectHistograms sends all known historams to prometheus
func (e *Exporter) collectHistograms(ch chan<- prometheus.Metric) {
	for _, program := range e.config.Programs {
		for _, histogram := range program.Metrics.Histograms {
			skip := false

			histograms := map[string]histogramWithLabels{}

			mapValues, err := e.mapValues(e.modules[program.Name], histogram.Name, histogram.Labels)
			if err != nil {
				log.Printf("Error getting map %q values for metric %q of program %q: %s", histogram.Name, histogram.Name, program.Name, err)
				continue
			}

			// Taking the last label and using int as bucket delimiter, for example:
			//
			// Before:
			// * [sda, read, 1ms] -> 10
			// * [sda, read, 2ms] -> 2
			// * [sda, read, 4ms] -> 5
			//
			// After:
			// * [sda, read] -> {1ms -> 10, 2ms -> 2, 4ms -> 5}
			for _, metricValue := range mapValues {
				labels := metricValue.labels[0 : len(metricValue.labels)-1]

				key := fmt.Sprintf("%#v", labels)

				if _, ok := histograms[key]; !ok {
					histograms[key] = histogramWithLabels{
						labels:  labels,
						buckets: map[float64]uint64{},
					}
				}

				leUint, err := strconv.ParseUint(metricValue.labels[len(metricValue.labels)-1], 0, 64)
				if err != nil {
					log.Printf("Error parsing float value for bucket %#v in map %q of program %q: %s", metricValue.labels, histogram.Name, program.Name, err)
					skip = true
					break
				}

				histograms[key].buckets[float64(leUint)] = uint64(metricValue.value)
			}

			if skip {
				continue
			}

			desc := e.descs[program.Name][histogram.Name]

			for _, histogramSet := range histograms {
				buckets, count, sum, err := transformHistogram(histogramSet.buckets, histogram)
				if err != nil {
					log.Printf("Error transforming histogram for metric %q in program %q: %s", histogram.Name, program.Name, err)
					continue
				}

				ch <- prometheus.MustNewConstHistogram(desc, count, sum, buckets, histogramSet.labels...)
			}
		}
	}
}

// mapValues returns values in the requested map to be used in metrics
func (e *Exporter) mapValues(module *libbpfgo.Module, name string, labels []config.Label) ([]metricValue, error) {
	values := []metricValue{}

	m, err := module.GetMap(name)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve map %q: %v", name, err)
	}

	keySize := uint(0)
	for _, label := range labels {
		keySize += label.Size
	}

	// If there are no labels, then just use key uint32(0)
	if len(labels) == 0 {
		key := []byte{0x0, 0x0, 0x0, 0x0}

		value, err := mapValue(m, unsafe.Pointer(&key[0]))
		if err != nil {
			return nil, err
		}

		return []metricValue{
			{
				raw:    key,
				labels: []string{},
				value:  value,
			},
		}, nil
	}

	iter := m.Iterator()

	for iter.Next() {
		key := iter.Key()

		mv := metricValue{
			raw:    key,
			labels: make([]string, len(labels)),
		}

		mv.labels, err = e.decoders.DecodeLabels(key, labels)
		if err != nil {
			if err == decoder.ErrSkipLabelSet {
				continue
			}

			return nil, err
		}

		mv.value, err = mapValue(m, unsafe.Pointer(&key[0]))
		if err != nil {
			return nil, err
		}

		values = append(values, mv)
	}

	return values, nil
}

func (e Exporter) exportMaps() (map[string]map[string][]metricValue, error) {
	maps := map[string]map[string][]metricValue{}

	for _, program := range e.config.Programs {
		module := e.modules[program.Name]
		if module == nil {
			return nil, fmt.Errorf("module for program %q is not attached", program.Name)
		}

		if _, ok := maps[program.Name]; !ok {
			maps[program.Name] = map[string][]metricValue{}
		}

		metricMaps := map[string][]config.Label{}

		for _, counter := range program.Metrics.Counters {
			if counter.Name != "" {
				metricMaps[counter.Name] = counter.Labels
			}
		}

		for _, histogram := range program.Metrics.Histograms {
			if histogram.Name != "" {
				metricMaps[histogram.Name] = histogram.Labels
			}
		}

		for name, labels := range metricMaps {
			metricValues, err := e.mapValues(e.modules[program.Name], name, labels)
			if err != nil {
				return nil, fmt.Errorf("error getting values for map %q of program %q: %s", name, program.Name, err)
			}

			maps[program.Name][name] = metricValues
		}
	}

	return maps, nil
}

// MapsHandler is a debug handler to print raw values of kernel maps
func (e *Exporter) MapsHandler(w http.ResponseWriter, r *http.Request) {
	maps, err := e.exportMaps()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Header().Add("Content-type", "text/plain")
		if _, err = fmt.Fprintf(w, "%s\n", err); err != nil {
			log.Printf("Error returning error to client %q: %s", r.RemoteAddr, err)
			return
		}
		return
	}

	w.Header().Add("Content-type", "text/plain")

	buf := []byte{}

	for program, maps := range maps {
		buf = append(buf, fmt.Sprintf("## Program: %s\n\n", program)...)

		for name, m := range maps {
			buf = append(buf, fmt.Sprintf("### Map: %s\n\n", name)...)

			buf = append(buf, "```\n"...)
			for _, row := range m {
				buf = append(buf, fmt.Sprintf("%#v (labels: %v) -> %f\n", row.raw, row.labels, row.value)...)
			}
			buf = append(buf, "```\n\n"...)
		}
	}

	if _, err = w.Write(buf); err != nil {
		log.Printf("Error returning map contents to client %q: %s", r.RemoteAddr, err)
	}
}

func mapValue(m *libbpfgo.BPFMap, key unsafe.Pointer) (float64, error) {
	v, err := m.GetValue(key)
	if err != nil {
		return 0.0, err
	}

	// Assuming counter's value type is always u64
	return float64(util.GetHostByteOrder().Uint64(v)), nil
}

// metricValue is a row in a kernel map
type metricValue struct {
	// raw is a raw key value provided by kernel
	raw []byte
	// labels are decoded from the raw key
	labels []string
	// value is the kernel map value
	value float64
}
