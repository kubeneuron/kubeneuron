package gpucatalog

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/metrics" // registers kubeneuron_* series at init
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// TestMain installs the two scrape-time collectors the controller registers
// at startup rather than at package init (kubeneuron_incidents and
// kubeneuron_degraded_gpus), with count functions that return nil, so the
// registry probe below sees the full set of series a live controller
// publishes. Registration is once per process and the counts never run.
func TestMain(m *testing.M) {
	metrics.RegisterIncidentStates(func() map[types.IncidentState]int { return nil })
	metrics.RegisterDegradedGPUs(func() map[metrics.DegradedKey]int { return nil })
	os.Exit(m.Run())
}

func TestDefaultValidates(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Version != Version || len(c.Fields) == 0 {
		t.Fatalf("unexpected catalog: version=%q fields=%d", c.Version, len(c.Fields))
	}
}

// The validator must reject each invariant breach, not just accept the
// built-in table: a validator that cannot fail proves nothing.
func TestValidateRejects(t *testing.T) {
	base := Default()
	cases := map[string]func(*Catalog){
		"T2 tier":         func(c *Catalog) { c.Fields[0].Tier = TierT2 },
		"T3 tier":         func(c *Catalog) { c.Fields[0].Tier = TierT3 },
		"profiling field": func(c *Catalog) { f := &c.Fields[0]; f.DCGMField = "DCGM_FI_PROF_SM_ACTIVE" },
		"profiling field with PROF id": func(c *Catalog) {
			f := &c.Fields[0]
			f.DCGMFieldID, f.Key, f.DCGMField = 1002, "dcgm:1002", "DCGM_FI_PROF_SM_ACTIVE"
			f.Series = f.DCGMField
			c.Fields = sortedByID(c.Fields)
		},
		"PCIe TX bytes under wrong id": func(c *Catalog) {
			c.Fields = dropSeries(c.Fields, BaselinePROFFields[1009], 0)
			f := &c.Fields[0]
			f.DCGMField, f.Series = BaselinePROFFields[1009], BaselinePROFFields[1009]
		},
		"PCIe TX bytes in T1": func(c *Catalog) { mutDCGM(c, 1009, func(f *Field) { f.Tier = TierT1 }) },
		"PCIe TX bytes as bytes": func(c *Catalog) {
			mutDCGM(c, 1009, func(f *Field) {
				f.Raw = raw(UnitBytes)
				f.Canonical.Unit = UnitBytes
				f.Canonical.Series = "kubeneuron_gpu_pcie_bytes"
			})
		},
		"PCIe RX bytes as counter": func(c *Catalog) {
			mutDCGM(c, 1010, func(f *Field) { f.Kind = KindCounter })
		},
		"PCIe TX bytes misnamed": func(c *Catalog) {
			mutDCGM(c, 1009, func(f *Field) { f.DCGMField = "DCGM_FI_DEV_PCIE_TX_BYTES" })
		},
		"recovery action exposed": func(c *Catalog) { mutDCGM(c, 1523, func(f *Field) { f.ExporterExposed = true }) },
		"recovery action consumed": func(c *Catalog) {
			mutDCGM(c, 1523, func(f *Field) { f.ConsumedBy = []string{"internal/controller"} })
		},
		"recovery action validated": func(c *Catalog) {
			mutDCGM(c, 1523, func(f *Field) {
				f.Support[HardwareT4] = SupportValidated
				f.Evidence = "a run, not v0.7"
			})
		},
		"exposed without shipped profile": func(c *Catalog) { mutDCGM(c, 190, func(f *Field) { f.ExporterExposed = true }) },
		"shipped profile without exposed": func(c *Catalog) { mutDCGM(c, 150, func(f *Field) { f.ExporterExposed = false }) },
		// --- profile = eligible set, both directions ---------------------------
		"eligible throttle field left unprofiled": func(c *Catalog) {
			mutDCGM(c, 242, func(f *Field) { f.ExporterExposed, f.Source = false, SourceDCGMField })
		},
		"eligible unrepairable flag left unprofiled": func(c *Catalog) {
			mutDCGM(c, 1507, func(f *Field) { f.ExporterExposed, f.Source = false, SourceDCGMField })
		},
		"eligible PCIe byte rate left unprofiled": func(c *Catalog) {
			mutDCGM(c, 1009, func(f *Field) { f.ExporterExposed, f.Source = false, SourceDCGMField })
		},
		"unmapped P-state profiled": func(c *Catalog) {
			mutDCGM(c, 190, func(f *Field) { f.ExporterExposed, f.Source = true, SourceDCGMExporter })
		},
		"sticky XID gauge profiled": func(c *Catalog) {
			mutDCGM(c, 230, func(f *Field) { f.ExporterExposed, f.Source = true, SourceDCGMExporter })
		},
		"recovery action profiled": func(c *Catalog) {
			mutDCGM(c, 1523, func(f *Field) { f.ExporterExposed, f.Source = true, SourceDCGMExporter })
		},
		"profiled field loses its canonical mapping": func(c *Catalog) {
			mutDCGM(c, 150, func(f *Field) { f.Canonical, f.CanonicalDeferred = nil, "dropped" })
		},
		"profiled field deferred": func(c *Catalog) { mutDCGM(c, 242, func(f *Field) { f.Deferred = "later" }) },
		// --- source honesty ---------------------------------------------------
		"unprofiled field claims dcgm-exporter": func(c *Catalog) { mutDCGM(c, 190, func(f *Field) { f.Source = SourceDCGMExporter }) },
		"unprofiled XID field claims dcgm-exporter": func(c *Catalog) {
			mutDCGM(c, 230, func(f *Field) { f.Source = SourceDCGMExporter })
		},
		"deferred recovery action claims dcgm-exporter": func(c *Catalog) {
			mutDCGM(c, 1523, func(f *Field) { f.Source = SourceDCGMExporter })
		},
		"profiled field with dcgm-field source": func(c *Catalog) { mutDCGM(c, 150, func(f *Field) { f.Source = SourceDCGMField }) },
		"profile listed as a consumer": func(c *Catalog) {
			mutDCGM(c, 150, func(f *Field) { f.ConsumedBy = append([]string{ShippedProfilePath}, f.ConsumedBy...) })
		},
		"DCGM field from the controller": func(c *Catalog) { c.Fields[0].Source = SourceController },
		"DCGM field with empty source":   func(c *Catalog) { c.Fields[0].Source = "" },
		"series alias collides":          func(c *Catalog) { c.Fields[0].SeriesAliases = []string{"DCGM_FI_DEV_MEM_CLOCK"} },
		"private evidence path": func(c *Catalog) {
			mutDCGM(c, 230, func(f *Field) { f.Evidence = "not v0.7: /home/someone/run.log" })
		},
		"evidence claims v0.7": func(c *Catalog) { mutDCGM(c, 230, func(f *Field) { f.Evidence = "validated in v0.7" }) },
		"T4 help names Tesla": func(c *Catalog) {
			for i := range c.Fields {
				if c.Fields[i].Tier == TierT4 {
					c.Fields[i].Help = "Tesla T4 control plane"
					return
				}
			}
		},
		"deferred list omits T3": func(c *Catalog) {
			var keep []Deferral
			for _, d := range c.Deferred {
				if !strings.Contains(d.Area, "T3") {
					keep = append(keep, d)
				}
			}
			c.Deferred = keep
		},
		"vgpu field": func(c *Catalog) { f := &c.Fields[0]; f.DCGMField = "DCGM_FI_DEV_VGPU_X" },
		"raw-only canonical": func(c *Catalog) {
			c.Fields[0].Canonical.Unit, c.Fields[0].Canonical.Factor = UnitMegahertz, 1
			c.Fields[0].Canonical.Series = "kubeneuron_gpu_clock_megahertz"
		},
		"factor 1 on conversion": func(c *Catalog) { c.Fields[0].Canonical.Factor = 1 },
		"factor on same unit":    func(c *Catalog) { mutDCGM(c, 150, func(f *Field) { f.Canonical.Factor = 2 }) },
		// --- canonical side ---------------------------------------------------
		"canonical outside kubeneuron_gpu_": func(c *Catalog) {
			mutDCGM(c, 150, func(f *Field) { f.Canonical.Series = "kubeneuron_temperature_celsius" })
		},
		"canonical dcgm-style name": func(c *Catalog) {
			mutDCGM(c, 150, func(f *Field) { f.Canonical.Series = "kubeneuron_gpu_GPU_TEMP_celsius" })
		},
		"counter missing _total": func(c *Catalog) {
			mutDCGM(c, 156, func(f *Field) { f.Canonical.Series = "kubeneuron_gpu_energy_joules" })
		},
		"gauge named _total": func(c *Catalog) {
			mutDCGM(c, 155, func(f *Field) { f.Canonical.Series = "kubeneuron_gpu_power_watts_total" })
		},
		"unit suffix missing": func(c *Catalog) {
			mutDCGM(c, 155, func(f *Field) { f.Canonical.Series = "kubeneuron_gpu_power" })
		},
		"unit suffix contradicts unit": func(c *Catalog) {
			mutDCGM(c, 155, func(f *Field) { f.Canonical.Series = "kubeneuron_gpu_power_celsius" })
		},
		"canonical kind differs from source": func(c *Catalog) {
			mutDCGM(c, 202, func(f *Field) { f.Kind = KindGauge })
		},
		"canonical unit outside set": func(c *Catalog) {
			mutDCGM(c, 150, func(f *Field) { f.Canonical.Unit = UnitPercent })
		},
		"canonical collision on labels": func(c *Catalog) {
			mutDCGM(c, 101, func(f *Field) { f.Canonical.Labels = []Label{L("domain", "sm")} })
		},
		"canonical collision unlabelled": func(c *Catalog) {
			mutDCGM(c, 251, func(f *Field) { f.Canonical.Series = "kubeneuron_gpu_memory_used_bytes" })
		},
		"canonical label names differ across rows": func(c *Catalog) {
			mutDCGM(c, 101, func(f *Field) { f.Canonical.Labels = []Label{L("kind", "memory")} })
		},
		"canonical value labels differ across rows": func(c *Catalog) {
			mutDCGM(c, 101, func(f *Field) {
				f.Canonical.Labels = nil
				f.Canonical.ValueLabels = []string{"domain"}
				f.Canonical.Factor = 0
				f.Canonical.Note = "decoded"
			})
		},
		"unknown label name": func(c *Catalog) {
			mutDCGM(c, 100, func(f *Field) { f.Canonical.Labels = []Label{L("clock", "sm")} })
		},
		"unstable identity label": func(c *Catalog) {
			mutDCGM(c, 150, func(f *Field) { f.Canonical.Labels = []Label{L("gpu_uuid", "fixed")} })
		},
		"label value not a token": func(c *Catalog) {
			mutDCGM(c, 100, func(f *Field) { f.Canonical.Labels = []Label{L("domain", "SM-clock")} })
		},
		"labels out of order": func(c *Catalog) {
			mutDCGM(c, 310, func(f *Field) { f.Canonical.Labels = []Label{L("scope", "volatile"), L("bits", "single")} })
		},
		"label both fixed and decoded": func(c *Catalog) {
			mutDCGM(c, 112, func(f *Field) { f.Canonical.Labels = []Label{L("reason", "any")} })
		},
		"decode with a factor":    func(c *Catalog) { mutDCGM(c, 112, func(f *Field) { f.Canonical.Factor = 1 }) },
		"decode without note":     func(c *Catalog) { mutDCGM(c, 112, func(f *Field) { f.Canonical.Note = "" }) },
		"unmapped without reason": func(c *Catalog) { mutDCGM(c, 190, func(f *Field) { f.CanonicalDeferred = "" }) },
		"mapped and unmapped at once": func(c *Catalog) {
			mutDCGM(c, 150, func(f *Field) { f.CanonicalDeferred = "also not mapped" })
		},
		"XID counter invented from sticky gauge": func(c *Catalog) {
			mutDCGM(c, 230, func(f *Field) {
				f.CanonicalDeferred = ""
				f.Canonical = &Canonical{Series: CanonicalXIDEventsTotal, Kind: KindCounter, Unit: UnitCount, Factor: 1}
			})
		},
		"XID counter with source retyped as counter": func(c *Catalog) {
			mutDCGM(c, 230, func(f *Field) {
				f.CanonicalDeferred = ""
				f.Kind = KindCounter
				f.Canonical = &Canonical{Series: CanonicalXIDEventsTotal, Kind: KindCounter, Unit: UnitCount, Factor: 1}
			})
		},
		"recovery action mapped": func(c *Catalog) {
			mutDCGM(c, 1523, func(f *Field) {
				f.CanonicalDeferred = ""
				f.Canonical = &Canonical{Series: CanonicalRecoveryAction, Kind: KindGauge, Unit: UnitCode, Factor: 1}
			})
		},
		"reserved list drops the XID counter": func(c *Catalog) {
			var keep []Reservation
			for _, r := range c.ReservedCanonical {
				if r.Series != CanonicalXIDEventsTotal {
					keep = append(keep, r)
				}
			}
			c.ReservedCanonical = keep
		},
		"contract statement altered": func(c *Catalog) { c.CanonicalContract = "every canonical series is emitted today" },
		"T4 canonical differs from series": func(c *Catalog) {
			for i := range c.Fields {
				if c.Fields[i].Tier == TierT4 {
					c.Fields[i].Canonical.Series = "kubeneuron_gpu_" + c.Fields[i].Series
					return
				}
			}
		},
		"T4 without canonical": func(c *Catalog) {
			for i := range c.Fields {
				if c.Fields[i].Tier == TierT4 {
					c.Fields[i].Canonical = nil
					return
				}
			}
		},
		"T4 in the gpu namespace": func(c *Catalog) {
			for i := range c.Fields {
				if c.Fields[i].Tier == TierT4 {
					s := "kubeneuron_gpu_zz_" + strings.TrimPrefix(c.Fields[i].Series, "kubeneuron_")
					c.Fields[i].Series, c.Fields[i].Key, c.Fields[i].Canonical.Series = s, "kubeneuron:"+s, s
					c.Fields = append(c.Fields[:i], append(c.Fields[i+1:], c.Fields[i])...)
					return
				}
			}
		},
		"validated without evidence": func(c *Catalog) {
			f := &c.Fields[0]
			f.Support = sup(SupportValidated, SupportDeclared, SupportDeclared)
			f.Evidence = ""
		},
		"evidence without validation": func(c *Catalog) { c.Fields[0].Evidence = "something" },
		"missing hardware column":     func(c *Catalog) { delete(c.Fields[0].Support, HardwareH100) },
		"duplicate dcgm id":           func(c *Catalog) { c.Fields = append(c.Fields[:1], c.Fields...) },
		"T4 with dcgm id": func(c *Catalog) {
			for i := range c.Fields {
				if c.Fields[i].Tier == TierT4 {
					c.Fields[i].DCGMFieldID = 999
					return
				}
			}
			t.Fatal("no T4 field")
		},
		"T4 from dcgm-exporter": func(c *Catalog) {
			for i := range c.Fields {
				if c.Fields[i].Tier == TierT4 {
					c.Fields[i].Source = SourceDCGMExporter
					return
				}
			}
		},
		"T4 from dcgm-field": func(c *Catalog) {
			for i := range c.Fields {
				if c.Fields[i].Tier == TierT4 {
					c.Fields[i].Source = SourceDCGMField
					return
				}
			}
		},
		"bucket overlap":   func(c *Catalog) { c.XIDBuckets[1].Codes = append(c.XIDBuckets[1].Codes, 13) },
		"no other bucket":  func(c *Catalog) { c.XIDBuckets = c.XIDBuckets[:len(c.XIDBuckets)-1] },
		"bucket names act": func(c *Catalog) { c.XIDBuckets[0].Name = "Drain Now" },
		"empty deferred":   func(c *Catalog) { c.Deferred = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := clone(base)
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("expected validation failure for %s", name)
			}
		})
	}
}

// mutDCGM applies fn to the entry with the given DCGM field id.
func mutDCGM(c *Catalog, id int, fn func(*Field)) {
	for i := range c.Fields {
		if c.Fields[i].DCGMFieldID == id {
			fn(&c.Fields[i])
			return
		}
	}
	panic("no such dcgm field in test catalog")
}

func sortedByID(fs []Field) []Field {
	sort.SliceStable(fs, func(i, j int) bool {
		if (fs[i].DCGMFieldID == 0) != (fs[j].DCGMFieldID == 0) {
			return fs[i].DCGMFieldID != 0
		}
		return fs[i].DCGMFieldID < fs[j].DCGMFieldID
	})
	return fs
}

// dropSeries removes every entry publishing series except one with keepID.
func dropSeries(fs []Field, series string, keepID int) []Field {
	out := fs[:0:0]
	for _, f := range fs {
		if f.Series == series && f.DCGMFieldID != keepID {
			continue
		}
		out = append(out, f)
	}
	return out
}

func clone(c Catalog) Catalog {
	out := c
	out.Fields = make([]Field, len(c.Fields))
	for i, f := range c.Fields {
		out.Fields[i] = f
		if f.Canonical != nil {
			k := *f.Canonical
			k.Labels = append([]Label(nil), f.Canonical.Labels...)
			k.ValueLabels = append([]string(nil), f.Canonical.ValueLabels...)
			out.Fields[i].Canonical = &k
		}
		if f.Support != nil {
			out.Fields[i].Support = map[Hardware]Support{}
			for k, v := range f.Support {
				out.Fields[i].Support[k] = v
			}
		}
	}
	out.XIDBuckets = make([]XIDBucket, len(c.XIDBuckets))
	for i, b := range c.XIDBuckets {
		out.XIDBuckets[i] = b
		out.XIDBuckets[i].Codes = append([]int(nil), b.Codes...)
	}
	out.Deferred = append([]Deferral(nil), c.Deferred...)
	out.ReservedCanonical = append([]Reservation(nil), c.ReservedCanonical...)
	out.Derived = make([]DerivedSignal, len(c.Derived))
	for i, d := range c.Derived {
		out.Derived[i] = d
		out.Derived[i].Labels = append([]Label(nil), d.Labels...)
		out.Derived[i].Carried = append([]string(nil), d.Carried...)
		out.Derived[i].Inputs = append([]string(nil), d.Inputs...)
	}
	return out
}

func TestTierSemantics(t *testing.T) {
	c := Default()
	// Tier by field id: T0 baseline, T1 reliability. Ids are the stable key;
	// names have been renamed by DCGM and are checked separately.
	baseline := []int{100, 101, 113, 114, 140, 150, 151, 153, 155, 156, 158, 159, 164, 190, 203, 204, 235, 236, 237, 238, 250, 251, 252, 1009, 1010}
	reliability := []int{112, 202, 230, 240, 241, 242, 243, 245, 310, 311, 312, 313, 390, 391, 392, 393, 394, 395, 396, 409, 419, 429, 439, 1507, 1523}
	for _, id := range baseline {
		if f, ok := c.LookupDCGM(id); !ok || f.Tier != TierT0 {
			t.Errorf("field %d: want T0, got %s (present=%v)", id, f.Tier, ok)
		}
	}
	for _, id := range reliability {
		if f, ok := c.LookupDCGM(id); !ok || f.Tier != TierT1 {
			t.Errorf("field %d: want T1, got %s (present=%v)", id, f.Tier, ok)
		}
	}
	if n := len(baseline) + len(reliability); n != countDCGM(c) {
		t.Errorf("DCGM entries: want %d listed here, catalog has %d", n, countDCGM(c))
	}
	for _, f := range c.Fields {
		if f.Tier == TierT4 && f.Source.IsDCGM() {
			t.Errorf("%s: T4 is control-plane, not DCGM", f.Series)
		}
		if f.Tier == TierT4 && f.Source != SourceController && f.Source != SourceAgent {
			t.Errorf("%s: T4 source must be the controller or the agent, got %s", f.Series, f.Source)
		}
		for _, tier := range DeferredTiers {
			if f.Tier == tier {
				t.Errorf("%s: tier %s is deferred and has no MVP entries", f.Series, tier)
			}
		}
		if strings.HasPrefix(f.DCGMField, "DCGM_FI_PROF_") && BaselinePROFFields[f.DCGMFieldID] != f.DCGMField {
			t.Errorf("%s: profiling field outside the PCIe byte-counter exception", f.DCGMField)
		}
	}
	for _, id := range []int{100, 156, 252, 203, 240} {
		f, _ := c.LookupDCGM(id)
		if f.Raw.Unit == f.Canonical.Unit {
			t.Errorf("field %d: expected a unit conversion, raw and canonical both %s", id, f.Raw.Unit)
		}
	}
	if f, _ := c.LookupDCGM(100); f.Canonical.Factor != 1e6 || f.Canonical.Unit != UnitHertz {
		t.Errorf("SM clock: MHz -> Hz factor wrong: %+v", *f.Canonical)
	}
	if f, _ := c.LookupDCGM(252); f.Canonical.Factor != 1<<20 || f.Canonical.Unit != UnitBytes {
		t.Errorf("FB used: MiB -> bytes factor wrong: %+v", *f.Canonical)
	}
	for _, id := range []int{240, 241, 242, 243, 245} {
		if f, _ := c.LookupDCGM(id); f.Raw.Unit != UnitNanoseconds || f.Canonical.Unit != UnitSeconds || f.Canonical.Factor != 1e-9 || f.Kind != KindCounter {
			t.Errorf("field %d: want nanoseconds -> seconds counter, factor 1e-9, got raw=%+v canonical=%+v kind=%s", id, f.Raw, f.Canonical, f.Kind)
		}
	}
}

// Source is honest about emission: dcgm-exporter exactly for the fields the
// shipped profile lists, dcgm-field for every other DCGM entry, and both
// populations are non-empty so the distinction is exercised.
func TestSourceMatchesProfile(t *testing.T) {
	c := Default()
	exporter, cataloged := 0, 0
	for _, f := range c.Fields {
		if f.DCGMFieldID == 0 {
			continue
		}
		switch {
		case f.ExporterExposed && f.Source == SourceDCGMExporter:
			exporter++
		case !f.ExporterExposed && f.Source == SourceDCGMField:
			cataloged++
		default:
			t.Errorf("field %d: exporter_exposed=%v but source=%s", f.DCGMFieldID, f.ExporterExposed, f.Source)
		}
	}
	if exporter == 0 || cataloged == 0 {
		t.Fatalf("both source populations must exist: dcgm-exporter=%d dcgm-field=%d", exporter, cataloged)
	}
	for _, id := range []int{190, 230, 1523} {
		if f, _ := c.LookupDCGM(id); f.Source != SourceDCGMField || f.ExporterExposed {
			t.Errorf("field %d is not in the shipped profile and must be source %s, got %s (exposed=%v)", id, SourceDCGMField, f.Source, f.ExporterExposed)
		}
	}
	for _, id := range []int{150, 242, 243, 245, 1009, 1010, 1507} {
		if f, _ := c.LookupDCGM(id); f.Source != SourceDCGMExporter || !f.ExporterExposed {
			t.Errorf("field %d is in the shipped profile and must be source %s, got %s (exposed=%v)", id, SourceDCGMExporter, f.Source, f.ExporterExposed)
		}
	}
	// The source split is exactly the eligibility rule: dcgm-exporter for
	// every eligible field, dcgm-field for the three that are not.
	for _, f := range c.Fields {
		if f.DCGMFieldID != 0 && f.ExporterExposed != ProfileEligible(f) {
			t.Errorf("field %d: exposed=%v eligible=%v", f.DCGMFieldID, f.ExporterExposed, ProfileEligible(f))
		}
	}
	if cataloged != 3 {
		t.Errorf("exactly 190, 230 and 1523 stay outside the profile, got %d unprofiled DCGM fields", cataloged)
	}
}

// The Appendix A throttle-time row is complete and finite: all five
// violation fields feed kubeneuron_gpu_throttle_seconds_total, each with its
// own reason, as nanosecond counters scaled to seconds. Removing any of them
// fails here, in TestAppendixAMappings and in TestTierSemantics.
func TestThrottleReasonsComplete(t *testing.T) {
	c := Default()
	want := map[int]string{240: "power", 241: "thermal", 242: "sync_boost", 243: "board_limit", 245: "reliability"}
	names := map[int]string{242: "DCGM_FI_DEV_SYNC_BOOST_VIOLATION", 243: "DCGM_FI_DEV_BOARD_LIMIT_VIOLATION", 245: "DCGM_FI_DEV_RELIABILITY_VIOLATION"}
	rows := c.LookupCanonical("kubeneuron_gpu_throttle_seconds_total")
	if len(rows) != len(want) {
		t.Fatalf("throttle series fed by %d rows, want %d", len(rows), len(want))
	}
	seen := map[string]bool{}
	for _, f := range rows {
		reason, ok := want[f.DCGMFieldID]
		if !ok {
			t.Errorf("field %d feeds the throttle series but is not an Appendix A violation field", f.DCGMFieldID)
			continue
		}
		if len(f.Canonical.Labels) != 1 || f.Canonical.Labels[0] != L("reason", reason) {
			t.Errorf("field %d: labels %v, want reason=%s", f.DCGMFieldID, f.Canonical.Labels, reason)
		}
		if f.Tier != TierT1 || f.Kind != KindCounter || f.Raw.Unit != UnitNanoseconds || f.Canonical.Unit != UnitSeconds || f.Canonical.Factor != 1e-9 || f.Canonical.Kind != KindCounter {
			t.Errorf("field %d: want T1 nanosecond counter -> seconds ×1e-9, got %+v canonical=%+v", f.DCGMFieldID, f, f.Canonical)
		}
		if !f.ExporterExposed || f.Source != SourceDCGMExporter || f.Deferred != "" || !reflect.DeepEqual(f.ConsumedBy, CanonicalRuleConsumerPaths) {
			t.Errorf("field %d: the reliability profile lists all five violation fields; must be profiled dcgm-exporter, not deferred and read only by the canonical recording rules: %+v", f.DCGMFieldID, f)
		}
		if n, ok := names[f.DCGMFieldID]; ok && (f.DCGMField != n || f.Series != n || len(f.Aliases) > 0) {
			t.Errorf("field %d: want official name %s, got field=%s series=%s aliases=%v", f.DCGMFieldID, n, f.DCGMField, f.Series, f.Aliases)
		}
		seen[reason] = true
	}
	for _, r := range want {
		if !seen[r] {
			t.Errorf("throttle reason %q is not fed by any field", r)
		}
	}
}

// Field 1507 is the Appendix A unrepairable-memory flag: a boolean gauge
// mapped to kubeneuron_gpu_memory_unrepairable, T1, profiled (read where
// the hardware reports it), read only by the canonical recording rules, and
// neither deferred nor validated.
func TestMemoryUnrepairable(t *testing.T) {
	f, ok := Default().LookupDCGM(1507)
	if !ok {
		t.Fatal("field 1507 missing")
	}
	// The header name is the field; the series is the pinned exporter's
	// go-dcgm spelling (_FLAG), the only name its CSV parser accepts.
	if f.DCGMField != "DCGM_FI_DEV_MEMORY_UNREPAIRABLE" || f.Series != "DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG" || f.Tier != TierT1 || f.Kind != KindGauge || f.Raw.Unit != UnitBool {
		t.Errorf("field 1507: want DCGM_FI_DEV_MEMORY_UNREPAIRABLE served as DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG, T1 boolean gauge, got %+v", f)
	}
	if f.Canonical == nil || f.Canonical.Series != "kubeneuron_gpu_memory_unrepairable" || f.Canonical.Kind != KindGauge || f.Canonical.Unit != UnitBool || f.Canonical.Factor != 1 || len(f.Canonical.Labels) != 0 {
		t.Errorf("field 1507: canonical %+v", f.Canonical)
	}
	if !f.ExporterExposed || f.Source != SourceDCGMExporter || f.Deferred != "" || !reflect.DeepEqual(f.ConsumedBy, CanonicalRuleConsumerPaths) || f.Evidence != "" {
		t.Errorf("field 1507 must be a profiled, undeferred dcgm-exporter entry read only by the canonical recording rules: %+v", f)
	}
	for hw, s := range f.Support {
		if s == SupportValidated {
			t.Errorf("field 1507 %s: no run has observed this field", hw)
		}
	}
	if !strings.Contains(f.Help, "1 = yes, 0 = no") {
		t.Errorf("field 1507 help must state the flag encoding: %q", f.Help)
	}
}

func countDCGM(c Catalog) int {
	n := 0
	for _, f := range c.Fields {
		if f.DCGMFieldID != 0 {
			n++
		}
	}
	return n
}

// The PCIe byte-rate fields are the one DCGM_FI_PROF_* exception: present,
// T0, bytes per second raw and canonical (a canonical unit, not raw-only),
// gauge (NVIDIA documents them as the interval-average bandwidth), and the
// ban on every other PROF field is proven by TestValidateRejects.
func TestPCIeByteRateException(t *testing.T) {
	c := Default()
	if !isCanonical(UnitBytesPerSec) {
		t.Fatal("bytes_per_second must be a canonical unit")
	}
	for id, name := range BaselinePROFFields {
		f, ok := c.LookupDCGM(id)
		if !ok {
			t.Fatalf("field %d (%s) missing", id, name)
		}
		if f.DCGMField != name || f.Series != name || f.Tier != TierT0 || f.Kind != KindGauge {
			t.Errorf("field %d: %+v", id, f)
		}
		if f.Raw.Unit != UnitBytesPerSec || f.Canonical == nil || f.Canonical.Unit != UnitBytesPerSec || f.Canonical.Factor != 1 {
			t.Errorf("field %d: want bytes_per_second raw and canonical, factor 1; got raw=%+v canonical=%+v", id, f.Raw, f.Canonical)
		}
		if f.Canonical.Series != "kubeneuron_gpu_pcie_bytes_per_second" || f.Canonical.Kind != KindGauge {
			t.Errorf("field %d: canonical %+v", id, *f.Canonical)
		}
		if !f.ExporterExposed || f.Source != SourceDCGMExporter || !reflect.DeepEqual(f.ConsumedBy, CanonicalRuleConsumerPaths) {
			t.Errorf("field %d: in the reliability profile and read only by the canonical recording rules, got exposed=%v source=%s consumers=%v", id, f.ExporterExposed, f.Source, f.ConsumedBy)
		}
		if strings.Contains(strings.ToLower(f.Help), "counter") && !strings.Contains(f.Help, "not a cumulative") {
			t.Errorf("field %d: help must not describe the field as a counter: %q", id, f.Help)
		}
	}
	if BaselinePROFFields[1009] != "DCGM_FI_PROF_PCIE_TX_BYTES" || BaselinePROFFields[1010] != "DCGM_FI_PROF_PCIE_RX_BYTES" {
		t.Fatalf("exception set drifted: %v", BaselinePROFFields)
	}
}

// Appendix A of the feature brief is the source of every canonical
// mapping. This pins the rows the brief spells out, label by label.
func TestAppendixAMappings(t *testing.T) {
	c := Default()
	type want struct {
		series string
		kind   Kind
		unit   Unit
		factor float64
		labels []Label
	}
	cases := map[int]want{
		100:  {"kubeneuron_gpu_clock_hertz", KindGauge, UnitHertz, 1e6, []Label{L("domain", "sm")}},
		101:  {"kubeneuron_gpu_clock_hertz", KindGauge, UnitHertz, 1e6, []Label{L("domain", "memory")}},
		113:  {"kubeneuron_gpu_clock_max_hertz", KindGauge, UnitHertz, 1e6, []Label{L("domain", "sm")}},
		114:  {"kubeneuron_gpu_clock_max_hertz", KindGauge, UnitHertz, 1e6, []Label{L("domain", "memory")}},
		140:  {"kubeneuron_gpu_memory_temperature_celsius", KindGauge, UnitCelsius, 1, nil},
		150:  {"kubeneuron_gpu_temperature_celsius", KindGauge, UnitCelsius, 1, nil},
		151:  {"kubeneuron_gpu_memory_temperature_limit_celsius", KindGauge, UnitCelsius, 1, nil},
		153:  {"kubeneuron_gpu_thermal_margin_celsius", KindGauge, UnitCelsius, 1, nil},
		155:  {"kubeneuron_gpu_power_watts", KindGauge, UnitWatts, 1, nil},
		156:  {"kubeneuron_gpu_energy_joules_total", KindCounter, UnitJoules, 1e-3, nil},
		158:  {"kubeneuron_gpu_temperature_limit_celsius", KindGauge, UnitCelsius, 1, []Label{L("limit", "slowdown")}},
		159:  {"kubeneuron_gpu_temperature_limit_celsius", KindGauge, UnitCelsius, 1, []Label{L("limit", "shutdown")}},
		164:  {"kubeneuron_gpu_power_limit_watts", KindGauge, UnitWatts, 1, nil},
		202:  {"kubeneuron_gpu_pcie_replays_total", KindCounter, UnitCount, 1, nil},
		203:  {"kubeneuron_gpu_utilization_ratio", KindGauge, UnitRatio, 1e-2, nil},
		204:  {"kubeneuron_gpu_memory_copy_utilization_ratio", KindGauge, UnitRatio, 1e-2, nil},
		235:  {"kubeneuron_gpu_pcie_link_generation", KindGauge, UnitCount, 1, []Label{L("kind", "max")}},
		236:  {"kubeneuron_gpu_pcie_link_width", KindGauge, UnitCount, 1, []Label{L("kind", "max")}},
		237:  {"kubeneuron_gpu_pcie_link_generation", KindGauge, UnitCount, 1, []Label{L("kind", "current")}},
		238:  {"kubeneuron_gpu_pcie_link_width", KindGauge, UnitCount, 1, []Label{L("kind", "current")}},
		240:  {"kubeneuron_gpu_throttle_seconds_total", KindCounter, UnitSeconds, 1e-9, []Label{L("reason", "power")}},
		241:  {"kubeneuron_gpu_throttle_seconds_total", KindCounter, UnitSeconds, 1e-9, []Label{L("reason", "thermal")}},
		242:  {"kubeneuron_gpu_throttle_seconds_total", KindCounter, UnitSeconds, 1e-9, []Label{L("reason", "sync_boost")}},
		243:  {"kubeneuron_gpu_throttle_seconds_total", KindCounter, UnitSeconds, 1e-9, []Label{L("reason", "board_limit")}},
		245:  {"kubeneuron_gpu_throttle_seconds_total", KindCounter, UnitSeconds, 1e-9, []Label{L("reason", "reliability")}},
		251:  {"kubeneuron_gpu_memory_free_bytes", KindGauge, UnitBytes, 1 << 20, nil},
		252:  {"kubeneuron_gpu_memory_used_bytes", KindGauge, UnitBytes, 1 << 20, nil},
		310:  {"kubeneuron_gpu_ecc_errors_total", KindCounter, UnitCount, 1, []Label{L("bits", "single"), L("scope", "volatile")}},
		311:  {"kubeneuron_gpu_ecc_errors_total", KindCounter, UnitCount, 1, []Label{L("bits", "double"), L("scope", "volatile")}},
		390:  {"kubeneuron_gpu_retired_pages_total", KindCounter, UnitCount, 1, []Label{L("cause", "single_bit")}},
		391:  {"kubeneuron_gpu_retired_pages_total", KindCounter, UnitCount, 1, []Label{L("cause", "double_bit")}},
		392:  {"kubeneuron_gpu_retired_pages_pending", KindGauge, UnitBool, 1, nil},
		393:  {"kubeneuron_gpu_remapped_rows_total", KindCounter, UnitCount, 1, []Label{L("kind", "uncorrectable")}},
		394:  {"kubeneuron_gpu_remapped_rows_total", KindCounter, UnitCount, 1, []Label{L("kind", "correctable")}},
		395:  {"kubeneuron_gpu_row_remap_failed", KindGauge, UnitBool, 1, nil},
		396:  {"kubeneuron_gpu_row_remap_pending", KindGauge, UnitBool, 1, nil},
		409:  {"kubeneuron_gpu_nvlink_errors_total", KindCounter, UnitCount, 1, []Label{L("type", "crc_flit")}},
		419:  {"kubeneuron_gpu_nvlink_errors_total", KindCounter, UnitCount, 1, []Label{L("type", "crc_data")}},
		429:  {"kubeneuron_gpu_nvlink_errors_total", KindCounter, UnitCount, 1, []Label{L("type", "replay")}},
		439:  {"kubeneuron_gpu_nvlink_errors_total", KindCounter, UnitCount, 1, []Label{L("type", "recovery")}},
		1009: {"kubeneuron_gpu_pcie_bytes_per_second", KindGauge, UnitBytesPerSec, 1, []Label{L("direction", "tx")}},
		1010: {"kubeneuron_gpu_pcie_bytes_per_second", KindGauge, UnitBytesPerSec, 1, []Label{L("direction", "rx")}},
		1507: {"kubeneuron_gpu_memory_unrepairable", KindGauge, UnitBool, 1, nil},
	}
	for id, w := range cases {
		f, ok := c.LookupDCGM(id)
		if !ok || f.Canonical == nil {
			t.Errorf("field %d: missing or unmapped", id)
			continue
		}
		k := f.Canonical
		if k.Series != w.series || k.Kind != w.kind || k.Unit != w.unit || k.Factor != w.factor {
			t.Errorf("field %d: got %s %s %s ×%v, want %s %s %s ×%v", id, k.Series, k.Kind, k.Unit, k.Factor, w.series, w.kind, w.unit, w.factor)
		}
		if len(k.Labels) != len(w.labels) {
			t.Errorf("field %d: labels %v, want %v", id, k.Labels, w.labels)
			continue
		}
		for i := range w.labels {
			if k.Labels[i] != w.labels[i] {
				t.Errorf("field %d: labels %v, want %v", id, k.Labels, w.labels)
			}
		}
	}
	// Clock-event reasons are a decode of the bitmask into per-reason
	// booleans, not a scalar conversion.
	f, _ := c.LookupDCGM(112)
	if f.Canonical == nil || f.Canonical.Series != "kubeneuron_gpu_clock_event_active" || f.Canonical.Unit != UnitBool ||
		len(f.Canonical.ValueLabels) != 1 || f.Canonical.ValueLabels[0] != "reason" || f.Canonical.Factor != 0 || len(f.Canonical.Labels) != 0 {
		t.Errorf("field 112: %+v", f.Canonical)
	}
	// Rows outside Appendix A must say so.
	for _, id := range []int{250, 312, 313} {
		f, _ := c.LookupDCGM(id)
		if f.Canonical == nil || !strings.Contains(f.Canonical.Note, "Appendix A") {
			t.Errorf("field %d: a mapping beyond Appendix A must carry a note saying so: %+v", id, f.Canonical)
		}
	}
	// Every DCGM entry is either mapped or explains why not.
	for _, f := range c.Fields {
		if f.DCGMFieldID == 0 {
			continue
		}
		if (f.Canonical == nil) == (f.CanonicalDeferred == "") {
			t.Errorf("field %d: exactly one of canonical / canonical_deferred", f.DCGMFieldID)
		}
		if f.Canonical != nil && !strings.HasPrefix(f.Canonical.Series, "kubeneuron_gpu_") {
			t.Errorf("field %d: canonical %s outside kubeneuron_gpu_", f.DCGMFieldID, f.Canonical.Series)
		}
	}
	for _, f := range c.Fields {
		if f.Tier == TierT4 && (f.Canonical == nil || f.Canonical.Series != f.Series) {
			t.Errorf("%s: control-plane entry must be its own canonical series", f.Series)
		}
	}
}

// The XID event counter is not invented from the sticky last-XID gauge: 230
// is unmapped with a reason naming the limitation, the counter's name is
// reserved, and nothing in the catalog claims it.
func TestXIDCounterNotInvented(t *testing.T) {
	c := Default()
	f, ok := c.LookupDCGM(230)
	if !ok || f.Kind != KindGauge || f.Canonical != nil {
		t.Fatalf("field 230 must stay an unmapped gauge: %+v", f)
	}
	for _, want := range []string{"sticky", CanonicalXIDEventsTotal, "reserved"} {
		if !strings.Contains(f.CanonicalDeferred, want) {
			t.Errorf("field 230 canonical_deferred must mention %q: %q", want, f.CanonicalDeferred)
		}
	}
	if len(c.LookupCanonical(CanonicalXIDEventsTotal)) != 0 || len(c.LookupCanonical(CanonicalRecoveryAction)) != 0 {
		t.Error("a reserved canonical series is claimed")
	}
	reserved := map[string]string{}
	for _, r := range c.ReservedCanonical {
		reserved[r.Series] = r.Reason
	}
	if !strings.Contains(reserved[CanonicalXIDEventsTotal], "DCGM_EXP_XID_ERRORS_TOTAL") {
		t.Errorf("reservation must name the counter source it waits for: %q", reserved[CanonicalXIDEventsTotal])
	}
	if reserved[CanonicalRecoveryAction] == "" {
		t.Error("recovery action canonical name must be reserved")
	}
	found := false
	for _, d := range c.Deferred {
		if strings.Contains(d.Area, CanonicalXIDEventsTotal) {
			found = true
		}
	}
	if !found {
		t.Error("deferred scope must name the XID event counter")
	}
}

// Canonical series never collide and the finite label set really is
// finite: every label name comes from CanonicalLabelNames and no label is
// a per-device or per-workload identity.
func TestCanonicalSeriesFinite(t *testing.T) {
	c := Default()
	seen := map[string]int{}
	for _, f := range c.Fields {
		if f.Canonical == nil || f.Tier == TierT4 {
			continue
		}
		key := f.Canonical.Series + f.Canonical.LabelSet()
		if prev, dup := seen[key]; dup {
			t.Errorf("%s fed by both field %d and field %d", key, prev, f.DCGMFieldID)
		}
		seen[key] = f.DCGMFieldID
		for _, l := range f.Canonical.Labels {
			if !isCanonicalLabel(l.Name) {
				t.Errorf("field %d: label %s outside the finite set", f.DCGMFieldID, l.Name)
			}
		}
	}
	for _, identity := range []string{"cluster", "node", "node_pool", "gpu_uuid", "gpu_model", "driver", "pod", "namespace", "container", "mig_profile", "instance", "job", "le", "quantile"} {
		if isCanonicalLabel(identity) {
			t.Errorf("identity label %s must never be a fixed canonical label", identity)
		}
	}
	if len(c.CanonicalSeries()) == 0 {
		t.Fatal("no canonical series")
	}
}

// A rule-file consumer claim must match the file: every field listing a
// rule file is referenced there by its source series, and every DCGM series
// a rule file references is cataloged with that consumer. The profile and
// the rules are separate claims; legacy() makes neither.
func TestRuleConsumersMatchFiles(t *testing.T) {
	root := repoRoot(t)
	c := Default()
	seriesRE := regexp.MustCompile(`DCGM_FI_[A-Z0-9_]+`)
	for _, path := range RuleConsumerPaths {
		src, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		inFile := map[string]bool{}
		for _, m := range seriesRE.FindAllString(string(src), -1) {
			inFile[m] = true
		}
		if len(inFile) == 0 {
			t.Fatalf("%s references no DCGM series; the harness is broken", path)
		}
		claimed := map[string]bool{}
		for _, f := range c.Fields {
			if hasConsumer(f, path) {
				claimed[f.Series] = true
				if !inFile[f.Series] {
					t.Errorf("%s claims %s reads it, but the file does not reference that series", f.Series, path)
				}
			}
		}
		for s := range inFile {
			if !claimed[s] {
				t.Errorf("%s references %s but no catalog entry lists the file as a consumer", path, s)
			}
		}
	}
	// Legacy names are a naming fact, not a consumer claim: most renamed
	// fields are in neither the profile nor the rules.
	unprofiledLegacy := 0
	for _, f := range c.Fields {
		if len(f.Aliases) > 0 && !f.ExporterExposed {
			unprofiledLegacy++
			for _, path := range RuleConsumerPaths {
				if hasConsumer(f, path) {
					t.Errorf("field %d: legacy name but unprofiled, must not claim %s", f.DCGMFieldID, path)
				}
			}
		}
	}
	if unprofiledLegacy == 0 {
		t.Fatal("expected renamed fields outside the shipped profile; the legacy/shipped distinction is untested")
	}
}

// Exporter exposure and rule consumption are independent claims. The
// reliability profile (v0.7 M2) lists every eligible field; the canonical
// recording rules (v0.7 M3) read every one of them, and the legacy raw
// alerts still read only the nine v0.6.1 series. The profile must never
// appear as a consumer, and a profiled field with no consumer must still
// validate and render honestly even though the shipped table no longer has
// one.
func TestProfiledWithoutRuleConsumerIsValid(t *testing.T) {
	// The constructors make each claim on its own.
	base := dcgm(150, "DCGM_FI_DEV_GPU_TEMP_CELSIUS", KindGauge, TierT0, UnitCelsius, "x", allDeclared)
	if p := profiled(base); !p.ExporterExposed || p.Source != SourceDCGMExporter || len(p.ConsumedBy) != 0 {
		t.Errorf("profiled() must set exposure and source and claim no consumer: %+v", p)
	}
	if r := readBy(base, LegacyRuleConsumerPaths...); r.ExporterExposed || r.Source != SourceDCGMField || !reflect.DeepEqual(r.ConsumedBy, LegacyRuleConsumerPaths) {
		t.Errorf("readBy() must record the rule files and leave exposure and source alone: %+v", r)
	}
	if r := recordedBy(base); !r.ExporterExposed || r.Source != SourceDCGMExporter || !reflect.DeepEqual(r.ConsumedBy, CanonicalRuleConsumerPaths) {
		t.Errorf("recordedBy() must profile the field and claim exactly the canonical rule files: %+v", r)
	}
	// Adding the legacy consumers on top repeats no path: the deployed
	// manifest is in both role sets but listed once.
	both := readBy(recordedBy(base), LegacyRuleConsumerPaths...)
	if want := []string{CanonicalRulePath, DeployedRulePath, LegacyRulePath}; !reflect.DeepEqual(both.ConsumedBy, want) {
		t.Errorf("readBy(recordedBy()) = %v, want %v", both.ConsumedBy, want)
	}
	if want := []string{LegacyRulePath, DeployedRulePath, CanonicalRulePath}; !reflect.DeepEqual(RuleConsumerPaths, want) {
		t.Errorf("RuleConsumerPaths = %v, want the de-duplicated union %v", RuleConsumerPaths, want)
	}
	for _, path := range RuleConsumerPaths {
		if path == ShippedProfilePath {
			t.Fatalf("%s is the exporter profile, not a rule file", path)
		}
	}

	// Present facts: the profiled population is the eligible set, none of it
	// lists the profile as a consumer, every profiled field is read by the
	// canonical recording rules, and exactly the nine v0.6.1 series are read
	// by the legacy raw alerts as well. M2 added exposure; M3 added the
	// recording rules as the consumer of every exposed field and nothing
	// else: the legacy alert file reads no new field.
	c := clone(Default())
	legacy := map[string]bool{}
	for _, s := range LegacyProfileSeries {
		legacy[s] = true
	}
	profiledIDs, canonicalOnly := 0, 0
	for _, f := range c.Fields {
		if !f.ExporterExposed {
			continue
		}
		profiledIDs++
		if hasConsumer(f, ShippedProfilePath) {
			t.Errorf("field %d lists the exporter profile as a consumer", f.DCGMFieldID)
		}
		for _, path := range CanonicalRuleConsumerPaths {
			if !hasConsumer(f, path) {
				t.Errorf("field %d (%s): every profiled field is recorded by %s, got %v", f.DCGMFieldID, f.Series, path, f.ConsumedBy)
			}
		}
		switch {
		case legacy[f.Series] && !hasConsumer(f, LegacyRulePath):
			t.Errorf("field %d (%s): the v0.6.1 series are still read by the legacy raw alerts, got %v", f.DCGMFieldID, f.Series, f.ConsumedBy)
		case !legacy[f.Series] && hasConsumer(f, LegacyRulePath):
			t.Errorf("field %d (%s): a series the reliability profile added is not read by the legacy raw alerts, got %v", f.DCGMFieldID, f.Series, f.ConsumedBy)
		case !legacy[f.Series]:
			canonicalOnly++
			if !reflect.DeepEqual(f.ConsumedBy, CanonicalRuleConsumerPaths) {
				t.Errorf("field %d (%s): consumers = %v, want exactly %v", f.DCGMFieldID, f.Series, f.ConsumedBy, CanonicalRuleConsumerPaths)
			}
		}
	}
	if profiledIDs != len(c.ProfileFields()) || profiledIDs <= len(LegacyProfileSeries) {
		t.Fatalf("profiled fields = %d, eligible = %d, legacy = %d", profiledIDs, len(c.ProfileFields()), len(LegacyProfileSeries))
	}
	if canonicalOnly == 0 {
		t.Fatal("expected profiled fields read only by the recording rules; the legacy/canonical consumer split is untested")
	}
	// A profiled field with no consumer is still a legitimate state: drop
	// field 242's consumers and the catalog validates with the field exposed
	// and its source unchanged.
	mutDCGM(&c, 242, func(f *Field) { f.ConsumedBy = nil })
	if err := c.Validate(); err != nil {
		t.Fatalf("a catalog with an unread profiled field must validate: %v", err)
	}
	if f, _ := c.LookupDCGM(242); !f.ExporterExposed || f.Source != SourceDCGMExporter || len(f.ConsumedBy) != 0 {
		t.Errorf("field 242: %+v", f)
	}

	// Dropping every consumer from a v0.6.1 field keeps the catalog valid
	// and the field exposed with source dcgm-exporter: exposure never
	// depends on consumption.
	mutDCGM(&c, 150, func(f *Field) { f.ConsumedBy = nil })
	if err := c.Validate(); err != nil {
		t.Fatalf("a profiled field with no rule consumer must validate: %v", err)
	}
	f, _ := c.LookupDCGM(150)
	if !f.ExporterExposed || f.Source != SourceDCGMExporter || len(f.ConsumedBy) != 0 {
		t.Errorf("exposure must not depend on consumers: %+v", f)
	}
	// Rendering such a catalog is still honest: the field is profiled in
	// every artifact and simply absent from the consumer list.
	rendered, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	md := string(rendered[PathMarkdown])
	if strings.Contains(md, "- `DCGM_FI_DEV_GPU_TEMP`:") {
		t.Error("markdown still lists a consumer for a field that claims none")
	}
	if !strings.Contains(md, "| 150 | `DCGM_FI_DEV_GPU_TEMP_CELSIUS (legacy: DCGM_FI_DEV_GPU_TEMP)` | dcgm-exporter | `DCGM_FI_DEV_GPU_TEMP` (alias `DCGM_FI_DEV_GPU_TEMP_CELSIUS`) | yes |") {
		t.Error("markdown no longer shows field 150 as profiled with source dcgm-exporter")
	}
	// The inverse decoupling holds too: a consumer claim does not make a
	// field exposed, so Validate still rejects the source/exposure mismatch
	// rather than inferring exposure from consumed_by.
	c2 := clone(Default())
	mutDCGM(&c2, 150, func(f *Field) { f.ExporterExposed = false })
	if err := c2.Validate(); err == nil {
		t.Error("field 150 with consumers but exposure dropped must fail source honesty, not pass via consumed_by")
	}
}

// Renamed DCGM fields: the current name is the field, the legacy name the
// shipped exporter profile uses is both an alias and the published series,
// and the current name is a series alias.
func TestLegacyNames(t *testing.T) {
	c := Default()
	cases := map[int][2]string{
		112: {"DCGM_FI_DEV_CLOCKS_EVENT_REASONS", "DCGM_FI_DEV_CLOCK_THROTTLE_REASONS"},
		140: {"DCGM_FI_DEV_MEMORY_TEMP_CELSIUS", "DCGM_FI_DEV_MEMORY_TEMP"},
		150: {"DCGM_FI_DEV_GPU_TEMP_CELSIUS", "DCGM_FI_DEV_GPU_TEMP"},
		151: {"DCGM_FI_DEV_MEMORY_MAX_OP_TEMP_CELSIUS", "DCGM_FI_DEV_MEM_MAX_OP_TEMP"},
		153: {"DCGM_FI_DEV_GPU_TEMP_MARGIN_CELSIUS", "DCGM_FI_DEV_GPU_TEMP_LIMIT"},
		155: {"DCGM_FI_DEV_BOARD_POWER_WATTS", "DCGM_FI_DEV_POWER_USAGE"},
		158: {"DCGM_FI_DEV_GPU_TEMP_SLOWDOWN_CELSIUS", "DCGM_FI_DEV_SLOWDOWN_TEMP"},
		159: {"DCGM_FI_DEV_GPU_TEMP_SHUTDOWN_CELSIUS", "DCGM_FI_DEV_SHUTDOWN_TEMP"},
		202: {"DCGM_FI_DEV_PCIE_REPLAY_TOTAL", "DCGM_FI_DEV_PCIE_REPLAY_COUNTER"},
		203: {"DCGM_FI_DEV_GPU_UTIL_RATIO", "DCGM_FI_DEV_GPU_UTIL"},
		230: {"DCGM_FI_DEV_XID_ERROR", "DCGM_FI_DEV_XID_ERRORS"},
		// The reverse direction: the header name is current, the pinned
		// go-dcgm (exporter 4.5.3-4.8.2) only knows the _FLAG spelling.
		1507: {"DCGM_FI_DEV_MEMORY_UNREPAIRABLE", "DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG"},
	}
	for id, want := range cases {
		f, ok := c.LookupDCGM(id)
		if !ok {
			t.Fatalf("field %d missing", id)
		}
		if f.DCGMField != want[0] || f.Series != want[1] {
			t.Errorf("field %d: field=%s series=%s, want %s / %s", id, f.DCGMField, f.Series, want[0], want[1])
		}
		if len(f.Aliases) != 1 || f.Aliases[0] != want[1] || len(f.SeriesAliases) != 1 || f.SeriesAliases[0] != want[0] {
			t.Errorf("field %d: aliases=%v series_aliases=%v", id, f.Aliases, f.SeriesAliases)
		}
		if g, ok := c.LookupSeries(want[1]); !ok || g.DCGMFieldID != id {
			t.Errorf("legacy series %s does not resolve to field %d", want[1], id)
		}
	}
}

// Field 1523 is cataloged only: deferred, not exposed, unknown everywhere,
// consumed by nothing, attached to no action.
func TestRecoveryActionCatalogedOnly(t *testing.T) {
	f, ok := Default().LookupDCGM(1523)
	if !ok || f.DCGMField != "DCGM_FI_DEV_GPU_RECOVERY_ACTION" || f.Tier != TierT1 {
		t.Fatalf("field 1523: %+v ok=%v", f, ok)
	}
	if f.Deferred == "" || f.ExporterExposed || f.Source != SourceDCGMField || len(f.ConsumedBy) > 0 || f.Evidence != "" || f.Canonical != nil {
		t.Errorf("field 1523 must be deferred, unexposed, dcgm-field, unconsumed, unmapped and without evidence: %+v", f)
	}
	for hw, s := range f.Support {
		if s != SupportUnknown {
			t.Errorf("field 1523 %s: want unknown, got %s", hw, s)
		}
	}
	// Parsed as the exporter parses it: a comment naming the field as
	// deliberately absent is fine, a data row is not.
	rows, err := ShippedProfile(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.Contains(r.Series, "RECOVERY_ACTION") {
			t.Errorf("shipped profile must not enable the recovery-action field: %+v", r)
		}
	}
}

// ExporterExposed is defined as "the shipped exporter profile lists the
// series": the set of exposed catalog series must equal the CSV field names
// in that profile, in both directions, with the catalog's kind on every row.
// The profile is parsed the way the pinned exporter parses it (values YAML,
// then CSV), not by grepping lines.
func TestExporterExposedMatchesShippedProfile(t *testing.T) {
	rows, err := ShippedProfile(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows parsed from the shipped profile; the harness is broken")
	}
	c := Default()
	inProfile := map[string]ProfileRow{}
	for _, r := range rows {
		if _, dup := inProfile[r.Series]; dup {
			t.Errorf("%s lists %s twice", ShippedProfilePath, r.Series)
		}
		inProfile[r.Series] = r
		f, ok := c.LookupSeries(r.Series)
		switch {
		case !ok:
			t.Errorf("%s lists %s, which is no catalog series (the exporter would be told to emit something the catalog does not describe)", ShippedProfilePath, r.Series)
		case !f.ExporterExposed:
			t.Errorf("%s lists %s but the catalog does not mark it exposed", ShippedProfilePath, r.Series)
		case r.Kind != f.Kind:
			t.Errorf("%s types %s as %s, catalog says %s", ShippedProfilePath, r.Series, r.Kind, f.Kind)
		}
	}
	for _, f := range c.Fields {
		if f.ExporterExposed {
			if _, ok := inProfile[f.Series]; !ok {
				t.Errorf("%s claims exporter exposure but %s does not list it", f.Series, ShippedProfilePath)
			}
		}
	}
}

// The shipped profile is exactly the eligible set: every T0/T1 field with a
// canonical mapping that is not deferred, under its exporter series name,
// and nothing else. Pinned ids: the five throttle counters, the PCIe byte
// rates and the unrepairable flag are in; the sticky XID gauge, the
// recovery action, the P-state, the NVLink byte rates (T2, not cataloged)
// and every other profiling or vGPU field are out.
func TestShippedProfileIsExactlyEligibleSet(t *testing.T) {
	c := Default()
	rows, err := ShippedProfile(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	inProfile := map[string]bool{}
	for _, r := range rows {
		inProfile[r.Series] = true
	}
	eligible := map[string]bool{}
	for _, f := range c.ProfileFields() {
		eligible[f.Series] = true
		if !inProfile[f.Series] {
			t.Errorf("eligible field %d (%s) is missing from %s", f.DCGMFieldID, f.Series, ShippedProfilePath)
		}
	}
	for s := range inProfile {
		if !eligible[s] {
			t.Errorf("%s lists %s, which is not an eligible catalog field", ShippedProfilePath, s)
		}
	}
	if len(rows) != len(c.ProfileFields()) || len(rows) == 0 {
		t.Fatalf("profile rows = %d, eligible fields = %d", len(rows), len(c.ProfileFields()))
	}
	// Pinned ids, by the series name the exporter uses for them.
	mustList := map[int]string{
		112: "DCGM_FI_DEV_CLOCK_THROTTLE_REASONS", 150: "DCGM_FI_DEV_GPU_TEMP", 155: "DCGM_FI_DEV_POWER_USAGE",
		240: "DCGM_FI_DEV_POWER_VIOLATION", 241: "DCGM_FI_DEV_THERMAL_VIOLATION",
		242: "DCGM_FI_DEV_SYNC_BOOST_VIOLATION", 243: "DCGM_FI_DEV_BOARD_LIMIT_VIOLATION", 245: "DCGM_FI_DEV_RELIABILITY_VIOLATION",
		1009: "DCGM_FI_PROF_PCIE_TX_BYTES", 1010: "DCGM_FI_PROF_PCIE_RX_BYTES", 1507: "DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG",
	}
	for id, series := range mustList {
		f, ok := c.LookupDCGM(id)
		if !ok || f.Series != series {
			t.Fatalf("field %d: catalog series %q, want %q", id, f.Series, series)
		}
		if !inProfile[series] {
			t.Errorf("field %d (%s) must be in the profile", id, series)
		}
	}
	mustNotList := map[int]string{190: "DCGM_FI_DEV_PSTATE", 230: "DCGM_FI_DEV_XID_ERRORS", 1523: "DCGM_FI_DEV_GPU_RECOVERY_ACTION"}
	for id, series := range mustNotList {
		f, ok := c.LookupDCGM(id)
		if !ok || f.Series != series {
			t.Fatalf("field %d: catalog series %q, want %q", id, f.Series, series)
		}
		if inProfile[series] {
			t.Errorf("field %d (%s) must not be in the profile", id, series)
		}
	}
	for _, uncataloged := range []string{"DCGM_FI_PROF_NVLINK_TX_BYTES", "DCGM_FI_PROF_NVLINK_RX_BYTES", "DCGM_FI_DEV_XID_ERROR", "DCGM_FI_DEV_PSTATE", "DCGM_FI_DRIVER_VERSION"} {
		if inProfile[uncataloged] {
			t.Errorf("%s must not be in the profile", uncataloged)
		}
	}
	for s := range inProfile {
		if strings.HasPrefix(s, "DCGM_FI_PROF_") && s != "DCGM_FI_PROF_PCIE_TX_BYTES" && s != "DCGM_FI_PROF_PCIE_RX_BYTES" {
			t.Errorf("profiling field %s (tier T2) must not be in the profile", s)
		}
		if strings.HasPrefix(s, "DCGM_FI_DEV_VGPU_") || strings.HasPrefix(s, "DCGM_EXP_") {
			t.Errorf("%s must not be in the profile", s)
		}
	}
	// The nine v0.6.1 series are a strict subset, so rollback is well-defined.
	for _, s := range LegacyProfileSeries {
		if !inProfile[s] {
			t.Errorf("v0.6.1 series %s must stay in the profile", s)
		}
	}
	if len(LegacyProfileSeries) != 9 || len(rows) <= 9 {
		t.Errorf("legacy = %d rows, profile = %d rows", len(LegacyProfileSeries), len(rows))
	}
}

// The v0.6.1 list is exactly the set of profiled series the legacy raw
// alerts read, so rolling the profile back to it leaves every alert's input
// in place; the series the reliability profile added are read only by the
// canonical recording rules, whose output would simply go absent.
func TestLegacyProfileSeriesAreTheRuleReaders(t *testing.T) {
	c := Default()
	legacy := map[string]bool{}
	for _, s := range LegacyProfileSeries {
		if legacy[s] {
			t.Errorf("legacy list repeats %s", s)
		}
		legacy[s] = true
		f, ok := c.LookupSeries(s)
		if !ok || !f.ExporterExposed {
			t.Errorf("legacy series %s must be a profiled catalog series", s)
		}
	}
	for _, f := range c.Fields {
		if f.DCGMFieldID == 0 {
			continue
		}
		reads := hasConsumer(f, LegacyRulePath)
		if reads != legacy[f.Series] {
			t.Errorf("field %d (%s): read by the legacy raw alerts = %v, in the v0.6.1 list = %v", f.DCGMFieldID, f.Series, reads, legacy[f.Series])
		}
		if legacy[f.Series] && !hasConsumer(f, DeployedRulePath) {
			t.Errorf("field %d (%s): the deployed manifest carries the legacy alerts too", f.DCGMFieldID, f.Series)
		}
	}
}

// ParseProfileCSV must accept what the exporter accepts and reject what it
// rejects, or the profile test proves nothing about the file the exporter
// will read.
func TestParseProfileCSV(t *testing.T) {
	rows, err := ParseProfileCSV("# Format: DCGM FIELD, Prometheus metric type, help message\n# comment, with, commas\nDCGM_FI_DEV_GPU_TEMP, gauge, GPU temperature in degrees Celsius.\n\nDCGM_FI_DEV_FB_USED,gauge,Framebuffer used in MiB.\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []ProfileRow{{"DCGM_FI_DEV_GPU_TEMP", KindGauge, "GPU temperature in degrees Celsius."}, {"DCGM_FI_DEV_FB_USED", KindGauge, "Framebuffer used in MiB."}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
	for name, text := range map[string]string{
		"comma in help":  "DCGM_FI_DEV_GPU_TEMP, gauge, GPU temperature, in degrees Celsius.\n",
		"missing help":   "DCGM_FI_DEV_GPU_TEMP, gauge\n",
		"empty kind":     "DCGM_FI_DEV_GPU_TEMP, , help\n",
		"quote in field": "DCGM_FI_DEV_GPU_TEMP, gauge, a \"quoted\" word\n",
	} {
		if _, err := ParseProfileCSV(text); err == nil {
			t.Errorf("%s: expected a parse error", name)
		}
	}
	// ProfileEligible is the rule the profile test uses; pin its three exclusions.
	c := Default()
	for id, want := range map[int]bool{100: true, 112: true, 190: false, 230: false, 242: true, 1009: true, 1507: true, 1523: false} {
		if f, _ := c.LookupDCGM(id); ProfileEligible(f) != want {
			t.Errorf("ProfileEligible(%d) = %v, want %v", id, !want, want)
		}
	}
	if ProfileEligible(cp("kubeneuron_incidents", SourceController, KindGauge, UnitCount, "x")) {
		t.Error("a control-plane series is never profile-eligible")
	}
}

// Hardware claims stay conservative: not-applicable only for the documented
// Turing-vs-Ampere feature splits, and the only validated cells cite the
// v0.6.0-era Tesla T4 run.
func TestHardwareClaimsConservative(t *testing.T) {
	notApplicableOK := map[int]bool{390: true, 391: true, 392: true, 393: true, 394: true, 395: true, 396: true, 409: true, 419: true, 429: true, 439: true}
	for _, f := range Default().Fields {
		if f.DCGMFieldID == 0 {
			continue
		}
		for hw, s := range f.Support {
			if s == SupportNotApplicable && !notApplicableOK[f.DCGMFieldID] {
				t.Errorf("field %d %s: not-applicable asserted without a documented feature split", f.DCGMFieldID, hw)
			}
			if s == SupportValidated && (hw != HardwareT4 || !strings.Contains(f.Evidence, "not v0.7")) {
				t.Errorf("field %d %s: validated cell outside the v0.6.0-era Tesla T4 evidence", f.DCGMFieldID, hw)
			}
		}
		if strings.Contains(f.Evidence, "TRANSFER_HANDOFF") {
			t.Errorf("field %d: evidence cites a handoff note rather than a public doc", f.DCGMFieldID)
		}
	}
	for id := range map[int]bool{140: true, 151: true} {
		f, _ := Default().LookupDCGM(id)
		if f.Support[HardwareT4] != SupportUnknown {
			t.Errorf("field %d tesla-t4: memory temperature support is unproven, want unknown, got %s", id, f.Support[HardwareT4])
		}
	}
}

// Field 230 is the one the agent's gpuhealth source polls by numeric id;
// the catalog must agree with the code that uses it.
func TestXIDFieldIDMatchesAgent(t *testing.T) {
	f, ok := Default().LookupDCGM(230)
	if !ok || f.DCGMField != "DCGM_FI_DEV_XID_ERROR" || f.Tier != TierT1 {
		t.Fatalf("field 230: %+v ok=%v", f, ok)
	}
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "internal/agent/gpuhealth/gpuhealth.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(src, []byte(`dcgmXIDFieldID = "230"`)) {
		t.Error("agent gpuhealth no longer polls field 230; the catalog evidence is stale")
	}
}

// Every XID the detector classifies has a named bucket, and no named bucket
// lists a code the detector does not know; "other" absorbs the rest.
func TestXIDBucketsMirrorDetectTable(t *testing.T) {
	known := map[int]bool{}
	for _, info := range detect.XIDTable() {
		known[info.Code] = true
		if b := BucketForXID(info.Code); b == XIDBucketOther {
			t.Errorf("detect XID %d has no bucket", info.Code)
		}
	}
	for _, b := range XIDBuckets {
		for _, code := range b.Codes {
			if !known[code] {
				t.Errorf("bucket %s lists XID %d which detect does not classify", b.Name, code)
			}
		}
	}
	if got := BucketForXID(9999); got != XIDBucketOther {
		t.Errorf("unknown code bucket = %q", got)
	}
	// Bucket names are label values: lower-case, no verbs that read as actions.
	for _, b := range XIDBuckets {
		for _, verb := range []string{"drain", "reset", "reboot", "replace", "cordon"} {
			if strings.Contains(b.Name, verb) {
				t.Errorf("bucket %q names an action", b.Name)
			}
		}
	}
}

// Every T4 series must be a metric the control plane or agent really
// registers, so the catalog cannot describe telemetry that does not exist.
func TestControlPlaneSeriesExist(t *testing.T) {
	cataloged := map[string]bool{}
	for _, f := range Default().Fields {
		if f.Tier != TierT4 {
			continue
		}
		cataloged[f.Series] = true
		if !seriesRegistered(f.Series) {
			t.Errorf("T4 series %s is not registered by internal/metrics", f.Series)
		}
	}
	if seriesRegistered("kubeneuron_definitely_not_a_metric") {
		t.Fatal("probe reports an unregistered series as registered")
	}
	// The scrape-time collectors (registered in TestMain) and the core
	// state / action / gate / capacity series must all be covered.
	for _, must := range []string{
		"kubeneuron_incidents", "kubeneuron_degraded_gpus", "kubeneuron_degraded_gpu_seconds_total",
		"kubeneuron_incidents_opened_total", "kubeneuron_incidents_recovered_total", "kubeneuron_incident_duration_seconds",
		"kubeneuron_actions_pending", "kubeneuron_steps_total", "kubeneuron_destructive_steps_deferred_total",
		"kubeneuron_gate_denials_total", "kubeneuron_escalations_total", "kubeneuron_workloads_evicted_total",
		"kubeneuron_agent_health_source", "kubeneuron_agent_detections_total",
	} {
		if !cataloged[must] {
			t.Errorf("T4 must cover existing series %s", must)
		}
	}
	// Sanity: the probe really distinguishes the dynamic collectors from an
	// unregistered name that merely looks like one.
	if !seriesRegistered("kubeneuron_incidents") || seriesRegistered("kubeneuron_incidents_diagnosed") {
		t.Fatal("registry probe does not see the scrape-time collectors installed in TestMain")
	}
}

// seriesRegistered probes the default registry: registering a throwaway
// collector under an existing name fails with a descriptor conflict, while a
// free name registers cleanly (and is unregistered again).
func seriesRegistered(name string) bool {
	probe := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "gpucatalog registry probe"})
	if err := prometheus.DefaultRegisterer.Register(probe); err != nil {
		return true
	}
	prometheus.DefaultRegisterer.Unregister(probe)
	return false
}

func TestRenderIsDeterministic(t *testing.T) {
	a, err := Render(Default())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(Default())
	if err != nil {
		t.Fatal(err)
	}
	for p := range a {
		if !bytes.Equal(a[p], b[p]) {
			t.Errorf("%s: two renders differ", p)
		}
	}
	var round yamlCatalog
	if err := yaml.Unmarshal(a[PathYAML], &round); err != nil {
		t.Fatalf("generated YAML does not parse: %v", err)
	}
	if round.Version != Version || len(round.Fields) != len(Fields) {
		t.Fatalf("YAML round-trip lost content: %+v", round.Version)
	}
	if round.CanonicalContract != CanonicalContract || len(round.ReservedCanonical) != len(ReservedCanonical) {
		t.Error("YAML must carry the canonical contract and the reserved names")
	}
	for _, f := range round.Fields {
		if f.DCGMFieldID == 100 && (f.Canonical == nil || f.Canonical.Series != "kubeneuron_gpu_clock_hertz" || len(f.Canonical.Labels) != 1 || f.Canonical.Labels[0] != L("domain", "sm")) {
			t.Errorf("YAML round-trip lost the canonical side of field 100: %+v", f.Canonical)
		}
	}
	md := string(a[PathMarkdown])
	for _, want := range []string{
		"No v0.7 hardware validation has been performed",
		"**T2** DCGM effective-work / profiling telemetry (deferred, no entries)",
		"**T3** serving / Triton telemetry (deferred, no entries)",
		"unrelated to the Tesla T4 GPU",
		"## Tier T2\n\nDeferred: no entries.",
		"## Tier T3\n\nDeferred: no entries.",
		"the contract the shipped recording rules implement",
		"A recorded series is not a validated one",
		"## Canonical series (recording-rule contract)",
		"| `kubeneuron_gpu_clock_hertz` | gauge | hertz | domain | 100: `DCGM_FI_DEV_SM_CLOCK` → `{domain=\"sm\"}`; 101: `DCGM_FI_DEV_MEM_CLOCK` → `{domain=\"memory\"}` | 100, 101 |",
		"| `kubeneuron_gpu_temperature_celsius` | gauge | celsius | — | 150: `DCGM_FI_DEV_GPU_TEMP` → (no labels) | 150 |",
		"| `kubeneuron_gpu_throttle_seconds_total` | counter | seconds | reason | 240: `DCGM_FI_DEV_POWER_VIOLATION` → `{reason=\"power\"}`; 241: `DCGM_FI_DEV_THERMAL_VIOLATION` → `{reason=\"thermal\"}`; 242: `DCGM_FI_DEV_SYNC_BOOST_VIOLATION` → `{reason=\"sync_boost\"}`; 243: `DCGM_FI_DEV_BOARD_LIMIT_VIOLATION` → `{reason=\"board_limit\"}`; 245: `DCGM_FI_DEV_RELIABILITY_VIOLATION` → `{reason=\"reliability\"}` | 240, 241, 242, 243, 245 |",
		"| `kubeneuron_gpu_memory_unrepairable` | gauge | boolean | — | 1507: `DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG` → (no labels) | 1507 |",
		"### DCGM fields with no canonical series",
		"- 230 `DCGM_FI_DEV_XID_ERRORS`: a sticky last-value gauge",
		"### Reserved canonical names",
		"- `kubeneuron_gpu_xid_errors_total`:",
		"| Source | Source series | Profiled |",
		"| dcgm-exporter | `DCGM_FI_DEV_POWER_USAGE` (alias `DCGM_FI_DEV_BOARD_POWER_WATTS`) | yes |",
		"| dcgm-exporter | `DCGM_FI_DEV_GPU_TEMP` (alias `DCGM_FI_DEV_GPU_TEMP_CELSIUS`) | yes |",
		"| dcgm-field | `DCGM_FI_DEV_XID_ERRORS` (alias `DCGM_FI_DEV_XID_ERROR`) | no |",
		"| 190 | `DCGM_FI_DEV_PSTATE` | dcgm-field | `DCGM_FI_DEV_PSTATE` | no | gauge | code | — | — | — |",
		"| 1507 | `DCGM_FI_DEV_MEMORY_UNREPAIRABLE (legacy: DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG)` | dcgm-exporter | `DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG` (alias `DCGM_FI_DEV_MEMORY_UNREPAIRABLE`) | yes | gauge | boolean | `kubeneuron_gpu_memory_unrepairable` | boolean | 1 |",
		"| 1523 | `DCGM_FI_DEV_GPU_RECOVERY_ACTION` | dcgm-field | `DCGM_FI_DEV_GPU_RECOVERY_ACTION` | no | gauge | code | — | — | — |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown must contain %q", want)
		}
	}
	for _, banned := range []string{"TRANSFER_HANDOFF", "Exporter series", "exporter-emitted", "emitted today by", "types them counter", "are not cataloged (sync-boost", "enabled in no shipped profile"} {
		if strings.Contains(md, banned) {
			t.Errorf("markdown must not contain %q", banned)
		}
	}
	// No unprofiled row may present itself as emitted by the exporter.
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "| dcgm-exporter |") && !strings.Contains(line, "| yes |") {
			t.Errorf("markdown row claims dcgm-exporter without being profiled: %s", line)
		}
		if strings.Contains(line, "| dcgm-field |") && strings.Contains(line, "| yes |") {
			t.Errorf("markdown row is profiled but not attributed to the exporter: %s", line)
		}
	}
	csv := string(a[PathCSV])
	if !strings.Contains(csv, "# deferred: DCGM_FI_DEV_GPU_RECOVERY_ACTION") || strings.Contains(csv, "\nDCGM_FI_DEV_GPU_RECOVERY_ACTION") {
		t.Error("CSV must list the recovery-action field only as a deferred comment")
	}
	for _, want := range []string{
		"# " + CSVReferenceNotice + "\n",
		"REFERENCE ONLY. This is NOT a dcgm-exporter metrics profile and NOT an inventory of emitted fields",
		"recorded when the source series exists, absent otherwise, and no evidence of hardware support either way",
		"DCGM_FI_DEV_SM_CLOCK, gauge, [T0][100] Current SM clock. | profiled=yes | source=dcgm-exporter | series=DCGM_FI_DEV_SM_CLOCK | canonical=kubeneuron_gpu_clock_hertz{domain=\"sm\"}",
		"DCGM_FI_DEV_GPU_TEMP_CELSIUS, gauge, [T0][150] GPU die temperature. | profiled=yes | source=dcgm-exporter | series=DCGM_FI_DEV_GPU_TEMP | canonical=kubeneuron_gpu_temperature_celsius",
		"DCGM_FI_DEV_PSTATE, gauge, [T0][190] Performance state (0 = max performance, 15 = idle). | profiled=no | source=dcgm-field | series=DCGM_FI_DEV_PSTATE | canonical=none",
		"DCGM_FI_DEV_SYNC_BOOST_VIOLATION, counter, [T1][242]",
		"| profiled=yes | source=dcgm-exporter | series=DCGM_FI_DEV_BOARD_LIMIT_VIOLATION | canonical=kubeneuron_gpu_throttle_seconds_total{reason=\"board_limit\"}",
		"| profiled=yes | source=dcgm-exporter | series=DCGM_FI_DEV_RELIABILITY_VIOLATION | canonical=kubeneuron_gpu_throttle_seconds_total{reason=\"reliability\"}",
		"DCGM_FI_DEV_MEMORY_UNREPAIRABLE, gauge, [T1][1507]",
		"| profiled=yes | source=dcgm-exporter | series=DCGM_FI_DEV_MEMORY_UNREPAIRABLE_FLAG | canonical=kubeneuron_gpu_memory_unrepairable\n",
		"| profiled=no | source=dcgm-field | series=DCGM_FI_DEV_XID_ERRORS | canonical=none (a sticky last-value gauge",
		"DCGM_FI_PROF_PCIE_TX_BYTES, gauge, [T0][1009]",
		"| profiled=yes | source=dcgm-exporter | series=DCGM_FI_PROF_PCIE_RX_BYTES |",
	} {
		if !strings.Contains(csv, want) {
			t.Errorf("CSV must contain %q", want)
		}
	}
	// Every data row states profiled and source consistently; a reader cannot
	// take the row list for an inventory of emitted fields.
	for _, line := range strings.Split(csv, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.Contains(line, "profiled=yes | source=dcgm-exporter |"), strings.Contains(line, "profiled=no | source=dcgm-field |"):
		default:
			t.Errorf("CSV row has an inconsistent profiled/source pair: %s", line)
		}
	}
	yamlText := string(a[PathYAML])
	for _, want := range []string{
		"canonical_contract: ", "reserved_canonical:", "  - series: kubeneuron_gpu_xid_errors_total", "canonical_deferred: ",
		"source is dcgm-exporter only for fields the shipped exporter profile lists",
		"    exporter_exposed: false\n    source: dcgm-field\n",
		"    exporter_exposed: true\n    source: dcgm-exporter\n",
	} {
		if !strings.Contains(yamlText, want) {
			t.Errorf("YAML must contain %q", want)
		}
	}
	for _, banned := range []string{"    exporter_exposed: false\n    source: dcgm-exporter\n", "    exporter_exposed: true\n    source: dcgm-field\n"} {
		if strings.Contains(yamlText, banned) {
			t.Errorf("YAML must not contain %q", banned)
		}
	}
}

// The checked-in artifacts must equal a fresh render; otherwise someone
// edited the table (or an artifact) without running the generator.
func TestCheckedInArtifactsMatch(t *testing.T) {
	root := repoRoot(t)
	rendered, err := Render(Default())
	if err != nil {
		t.Fatal(err)
	}
	paths := Paths()
	sort.Strings(paths)
	for _, p := range paths {
		got, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("%s: %v (run `make generate-gpucatalog`)", p, err)
		}
		if !bytes.Equal(got, rendered[p]) {
			t.Errorf("%s is stale; run `make generate-gpucatalog`", p)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// derivedIndex returns the index of the one row that records series with
// exactly the given fixed-label selector (empty for unlabeled rows).
func derivedIndex(t *testing.T, c Catalog, series, labelSet string) int {
	t.Helper()
	for i, d := range c.Derived {
		if d.Series == series && d.LabelSet() == labelSet {
			return i
		}
	}
	t.Fatalf("no derived row %s%s", series, labelSet)
	return -1
}

// The derived validator accepts the one shipped derived-reads-derived
// dependency (the fail-slow score reads the clock and throttle ratios
// declared above it) and rejects every way of pointing a formula at a
// series that is not an honest, already-evaluated catalog input.
func TestValidateDerivedInputs(t *testing.T) {
	base := Default()
	const failSlow, clock, power = "kubeneuron_gpu_fail_slow_score_ratio", "kubeneuron_gpu_clock_ratio", "kubeneuron_gpu_power_ratio"
	fs := base.Derived[derivedIndex(t, base, failSlow, "")]
	if want := []string{clock, "kubeneuron_gpu_throttle_share_ratio"}; !reflect.DeepEqual(fs.Inputs, want) {
		t.Fatalf("fail-slow inputs %v, want the two earlier derived ratios %v", fs.Inputs, want)
	}
	if derivedIndex(t, base, clock, "") >= derivedIndex(t, base, failSlow, "") {
		t.Fatal("the clock ratio must be declared before the fail-slow score that reads it")
	}
	cases := map[string]func(c *Catalog){
		"unknown input": func(c *Catalog) {
			i := derivedIndex(t, *c, power, "")
			c.Derived[i].Inputs = append(c.Derived[i].Inputs, "kubeneuron_gpu_power_cap_watts")
			c.Derived[i].Formula += " and kubeneuron_gpu_power_cap_watts"
		},
		"self input": func(c *Catalog) {
			i := derivedIndex(t, *c, power, "")
			c.Derived[i].Inputs = append(c.Derived[i].Inputs, power)
			c.Derived[i].Formula += " and " + power
		},
		"forward derived input": func(c *Catalog) {
			// The power ratio precedes the fail-slow score; reading the
			// score from it is a forward reference.
			i := derivedIndex(t, *c, power, "")
			c.Derived[i].Inputs = append(c.Derived[i].Inputs, failSlow)
			c.Derived[i].Formula += " and " + failSlow
		},
		"later derived input via reorder": func(c *Catalog) {
			// Move the fail-slow row ahead of the clock ratio it reads.
			i, j := derivedIndex(t, *c, clock, ""), derivedIndex(t, *c, failSlow, "")
			c.Derived[i], c.Derived[j] = c.Derived[j], c.Derived[i]
		},
		"or vector(0)": func(c *Catalog) {
			i := derivedIndex(t, *c, power, "")
			c.Derived[i].Formula = "(" + c.Derived[i].Formula + ") or vector(0)"
		},
		"input not in formula": func(c *Catalog) {
			i := derivedIndex(t, *c, power, "")
			c.Derived[i].Inputs = append(c.Derived[i].Inputs, "kubeneuron_gpu_temperature_celsius")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := clone(base)
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("expected validation failure for %s", name)
			}
		})
	}
}

// Memory-error pressure coverage, checked against the catalog mappings and
// formulas, not against prose: the corrected rate covers 310 and 312 and
// the uncorrectable rate covers 311 and 313 by carrying scope; each
// uncorrectable counter (311 volatile, 313 aggregate) is its own hard-fault
// component reading exactly its scoped selector; the row-remap flags 395
// and 396 are components with their own inputs; and no hard-fault formula
// aggregates or combines anything.
func TestDerivedMemoryCoverage(t *testing.T) {
	c := Default()
	// Which DCGM ids map to kubeneuron_gpu_ecc_errors_total with which bits
	// and scope.
	byBits := map[string][]int{}
	bySelector := map[string]int{}
	for _, f := range c.LookupCanonical("kubeneuron_gpu_ecc_errors_total") {
		ls := labelsOf(f.Canonical.Labels)
		byBits[ls["bits"]] = append(byBits[ls["bits"]], f.DCGMFieldID)
		bySelector[`bits="`+ls["bits"]+`",scope="`+ls["scope"]+`"`] = f.DCGMFieldID
	}
	sort.Ints(byBits["single"])
	sort.Ints(byBits["double"])
	if !reflect.DeepEqual(byBits["single"], []int{310, 312}) || !reflect.DeepEqual(byBits["double"], []int{311, 313}) {
		t.Fatalf("ECC mapping: single %v double %v; want 310+312 and 311+313", byBits["single"], byBits["double"])
	}
	if bySelector[`bits="double",scope="volatile"`] != 311 || bySelector[`bits="double",scope="aggregate"`] != 313 {
		t.Fatalf("ECC scope mapping %v; want 311 volatile and 313 aggregate", bySelector)
	}
	for _, tc := range []struct{ series, bits string }{
		{"kubeneuron_gpu_ecc_corrected_errors_per_second", "single"},
		{"kubeneuron_gpu_ecc_uncorrectable_errors_per_second", "double"},
	} {
		d := c.Derived[derivedIndex(t, c, tc.series, "")]
		if !reflect.DeepEqual(d.Inputs, []string{"kubeneuron_gpu_ecc_errors_total"}) {
			t.Errorf("%s inputs %v", tc.series, d.Inputs)
		}
		if !reflect.DeepEqual(d.Carried, []string{"scope"}) {
			t.Errorf("%s carries %v, want scope so both %v are covered", tc.series, d.Carried, byBits[tc.bits])
		}
		if want := `kubeneuron_gpu_ecc_errors_total{bits="` + tc.bits + `"}`; !strings.Contains(d.Formula, want) || strings.Contains(d.Formula, "scope=") {
			t.Errorf("%s formula %q must select %s and not fix scope", tc.series, d.Formula, want)
		}
	}
	const hardFault = "kubeneuron_gpu_memory_hard_fault_active"
	components := map[string]struct {
		input    string
		id       int
		selector string // the fixed-label selector the formula must apply to the input, if any
	}{
		"uncorrectable_ecc_volatile":  {"kubeneuron_gpu_ecc_errors_total", 311, `{bits="double",scope="volatile"}`},
		"uncorrectable_ecc_aggregate": {"kubeneuron_gpu_ecc_errors_total", 313, `{bits="double",scope="aggregate"}`},
		"row_remap_failed":            {"kubeneuron_gpu_row_remap_failed", 395, ""},
		"row_remap_pending":           {"kubeneuron_gpu_row_remap_pending", 396, ""},
	}
	for comp, want := range components {
		d := c.Derived[derivedIndex(t, c, hardFault, `{component="`+comp+`"}`)]
		if !reflect.DeepEqual(d.Inputs, []string{want.input}) {
			t.Errorf("component %s inputs %v, want [%s]", comp, d.Inputs, want.input)
		}
		f, ok := c.LookupDCGM(want.id)
		if !ok || f.Canonical == nil || f.Canonical.Series != want.input {
			t.Errorf("field %d does not map to %s", want.id, want.input)
		}
		// The formula reads exactly the one (scoped) counter and the
		// selector names the field the component claims.
		if errs := hardFaultComponentDefects(d.Formula, want.input, want.selector); len(errs) != 0 {
			t.Errorf("component %s formula %q: %v", comp, d.Formula, errs)
		}
		if want.selector != "" {
			if got := bySelector[strings.Trim(want.selector, "{}")]; got != want.id {
				t.Errorf("component %s selects %s, which is field %d, want %d", comp, want.selector, got, want.id)
			}
		}
		if !strings.Contains(d.NoData, fmt.Sprintf("(%d)", want.id)) {
			t.Errorf("component %s no_data %q does not name field %d", comp, d.NoData, want.id)
		}
	}
	// Every hard-fault component is a boolean over one input with no
	// aggregation, no `or` fallback and no arithmetic between series; the
	// component label values are distinct and no bare "uncorrectable_ecc"
	// row remains to be mistaken for both counters.
	seen := map[string]bool{}
	for _, d := range c.LookupDerived(hardFault) {
		comp := labelsOf(d.Labels)["component"]
		if comp == "" || seen[comp] {
			t.Errorf("hard-fault row %+v: component label missing or repeated", d)
		}
		seen[comp] = true
		if len(d.Inputs) != 1 || d.Unit != UnitBool {
			t.Errorf("component %s: inputs %v unit %s, want one input and a boolean", comp, d.Inputs, d.Unit)
			continue
		}
		if errs := hardFaultComponentDefects(d.Formula, d.Inputs[0], ""); len(errs) != 0 {
			t.Errorf("component %s formula %q: %v", comp, d.Formula, errs)
		}
	}
	if seen["uncorrectable_ecc"] {
		t.Error("a bare uncorrectable_ecc component remains; the counters are covered by uncorrectable_ecc_volatile and uncorrectable_ecc_aggregate")
	}
	for comp := range components {
		if !seen[comp] {
			t.Errorf("component %s is not recorded", comp)
		}
	}
	if !seen["unrepairable"] {
		t.Error("component unrepairable (1507) is not recorded")
	}
}

// The coverage check above is mechanical: a hard-fault component that reads
// the wrong scope, the unscoped counter or combines two counters fails the
// same assertions when applied to a mutated row.
func TestDerivedMemoryCoverageRejectsDrift(t *testing.T) {
	const hardFault = "kubeneuron_gpu_memory_hard_fault_active"
	base := Default()
	i := derivedIndex(t, base, hardFault, `{component="uncorrectable_ecc_aggregate"}`)
	ok := base.Derived[i].Formula
	cases := map[string]string{
		"wrong scope":    strings.Replace(ok, `scope="aggregate"`, `scope="volatile"`, 1),
		"unscoped":       strings.Replace(ok, `{bits="double",scope="aggregate"}`, `{bits="double"}`, 1),
		"summed scopes":  `sum(label_del(increase(kubeneuron_gpu_ecc_errors_total{bits="double"}[1h]), "bits", "scope")) > bool 0`,
		"or fallback":    `(` + ok + `) or (label_del(increase(kubeneuron_gpu_ecc_errors_total{bits="double",scope="volatile"}[1h]), "bits", "scope") > bool 0)`,
		"not a boolean":  strings.TrimSuffix(ok, " > bool 0"),
		"double counted": strings.Replace(ok, `scope="aggregate"}[1h])`, `scope="aggregate"}[1h]) + increase(kubeneuron_gpu_ecc_errors_total{bits="double",scope="aggregate"}[1h])`, 1),
	}
	for name, formula := range cases {
		t.Run(name, func(t *testing.T) {
			if formula == ok {
				t.Fatal("mutation did not change the formula; the case is broken")
			}
			if errs := hardFaultComponentDefects(formula, "kubeneuron_gpu_ecc_errors_total", `{bits="double",scope="aggregate"}`); len(errs) == 0 {
				t.Fatalf("mutated formula %q accepted", formula)
			}
		})
	}
	if errs := hardFaultComponentDefects(ok, "kubeneuron_gpu_ecc_errors_total", `{bits="double",scope="aggregate"}`); len(errs) != 0 {
		t.Fatalf("shipped formula rejected: %v", errs)
	}
}

// hardFaultComponentDefects is the pure core of the per-component formula
// assertions in TestDerivedMemoryCoverage, so a mutation can be judged
// without a Catalog: the formula reads input exactly once, with selector
// when one is given (so the counter is never read unscoped or with another
// scope), is a `> bool 0` boolean, and aggregates or combines nothing.
func hardFaultComponentDefects(formula, input, selector string) []string {
	var out []string
	if n := strings.Count(formula, input); n != 1 {
		out = append(out, fmt.Sprintf("reads %s %d times, want exactly once", input, n))
	}
	if selector != "" && !strings.Contains(formula, input+selector) {
		out = append(out, fmt.Sprintf("does not read %s%s", input, selector))
	}
	if !strings.HasSuffix(formula, "> bool 0") {
		out = append(out, "not a `> bool 0` boolean")
	}
	for _, bad := range []string{"sum", "avg", "count", "min", "max", "median", "quantile", " or ", " and ", " + ", " unless "} {
		if strings.Contains(formula, bad) {
			out = append(out, fmt.Sprintf("contains %q", bad))
		}
	}
	return out
}

// The fail-slow shadow signal's contract, checked field by field: one
// experimental derived row, gated and deduplicated as stated, reading the
// derived clock and throttle ratios, touching no optional label and
// synthesising nothing. The no_data text names every case of absence.
func TestFailSlowContract(t *testing.T) {
	c := Default()
	const series = "kubeneuron_gpu_fail_slow_score_ratio"
	rows := c.LookupDerived(series)
	if len(rows) != 1 {
		t.Fatalf("%d rows record %s, want exactly 1", len(rows), series)
	}
	d := rows[0]
	if !d.Experimental || d.Group != DerivedGroupFailSlow || d.Deferred != "" || len(d.Labels) != 0 {
		t.Errorf("row %+v: want experimental, group %s, no deferral, no fixed labels", d, DerivedGroupFailSlow)
	}
	if !reflect.DeepEqual(d.Inputs, []string{"kubeneuron_gpu_clock_ratio", "kubeneuron_gpu_throttle_share_ratio"}) {
		t.Errorf("inputs %v", d.Inputs)
	}
	for _, want := range []string{
		`median by (gpu_model) (max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_clock_ratio{domain="sm"}))`,
		`>= 0.15`, `>= 0.05`, `>= 3`,
		`max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_throttle_share_ratio)`,
		`count by (gpu_model) (max by (node, gpu_uuid, gpu_model) (kubeneuron_gpu_clock_ratio{domain="sm"}))`,
	} {
		if !strings.Contains(d.Formula, want) {
			t.Errorf("formula lacks %q", want)
		}
	}
	for _, bad := range []string{"cluster", "node_pool", "driver", "or vector(0)", "default", "absent("} {
		if strings.Contains(d.Formula, bad) {
			t.Errorf("formula contains %q", bad)
		}
	}
	for _, want := range []string{"shadow only", "trigger no remediation", "at least 3 distinct devices", "either input is missing", "has no sample, not 0"} {
		if !strings.Contains(d.NoData, want) {
			t.Errorf("no_data lacks %q", want)
		}
	}
	// The rule file carries it as one recording rule, no alert, no for.
	n := 0
	for _, g := range derivedGroups(t) {
		for _, r := range g.Rules {
			if r.Record != series {
				continue
			}
			n++
			if g.Name != DerivedGroupFailSlow || r.Alert != "" || r.For != "" || r.Expr != d.Formula || len(r.Labels) != 0 {
				t.Errorf("rule in %s: %+v", g.Name, r)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d rules record %s, want 1", n, series)
	}
}
