package detect

import (
	"fmt"
	"strconv"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Catalog classifies signals using the built-in tables plus declarative
// overrides from GPUSignalMapping resources. Overrides win over built-ins;
// built-ins remain for everything not overridden.
type Catalog struct {
	xid    map[int]XIDInfo
	alerts map[string]alertOverride
}

type alertOverride struct {
	class    types.ProblemClass
	severity types.Severity
}

// NewCatalog builds a catalog from validated overrides.
func NewCatalog(overrides []config.SignalOverride) (*Catalog, error) {
	c := &Catalog{
		xid:    map[int]XIDInfo{},
		alerts: map[string]alertOverride{},
	}
	for _, o := range overrides {
		if err := o.Validate(); err != nil {
			return nil, err
		}
		if o.AlertName != "" {
			if _, dup := c.alerts[o.AlertName]; dup {
				return nil, fmt.Errorf("signal override %q: alert %q is already overridden", o.Name, o.AlertName)
			}
			c.alerts[o.AlertName] = alertOverride{class: o.Class, severity: o.Severity}
			continue
		}
		for _, code := range o.XIDCodes {
			if _, dup := c.xid[code]; dup {
				return nil, fmt.Errorf("signal override %q: XID %d is already overridden", o.Name, code)
			}
			c.xid[code] = XIDInfo{
				Code:        code,
				Name:        o.Name,
				Class:       o.Class,
				Severity:    o.Severity,
				Description: "declarative override (GPUSignalMapping " + o.Name + ")",
			}
		}
	}
	return c, nil
}

// ClassifyXID resolves an XID against overrides first, then the built-in
// table.
func (c *Catalog) ClassifyXID(code int) (XIDInfo, bool) {
	if c != nil && c.xid != nil {
		if info, ok := c.xid[code]; ok {
			return info, true
		}
	}
	return ClassifyXID(code)
}

// ObservePolicy reports the catalog's observation threshold for a problem
// class: how many occurrences within a rolling window make the class
// actionable. It resolves overrides first, then the built-in table. When
// several XID entries share the class, the largest threshold wins — more
// observation before automated action is the fail-closed direction. ok is
// false when no entry declares a threshold for the class.
func (c *Catalog) ObservePolicy(class types.ProblemClass) (threshold int, window time.Duration, ok bool) {
	consider := func(info XIDInfo) {
		if info.Class != class || info.Threshold <= 0 {
			return
		}
		w, err := time.ParseDuration(info.ThresholdWindow)
		if err != nil || w <= 0 {
			return
		}
		if info.Threshold > threshold {
			threshold, window, ok = info.Threshold, w, true
		}
	}
	overridden := map[int]bool{}
	if c != nil {
		for code, info := range c.xid {
			overridden[code] = true
			consider(info)
		}
	}
	for code, info := range xidTable {
		if !overridden[code] {
			consider(info)
		}
	}
	return threshold, window, ok
}

// SignalFromAgentEvent converts an agent XID event into a Signal using this
// catalog, or ok=false when the XID is not actionable.
func (c *Catalog) SignalFromAgentEvent(ev types.AgentEvent) (types.Signal, bool) {
	info, ok := c.ClassifyXID(ev.XID)
	if !ok {
		return types.Signal{}, false
	}
	return types.Signal{
		Target: types.Target{
			Node:     ev.Node,
			GPUUUID:  ev.GPUUUID,
			GPUIndex: ev.GPUIndex,
		},
		Class:    info.Class,
		Severity: info.Severity,
		Source:   types.SourceAgentEvent,
		Evidence: map[string]string{
			"xid":      strconv.Itoa(ev.XID),
			"xid_name": info.Name,
			"raw":      ev.Raw,
		},
		ObservedAt: ev.Timestamp,
	}, true
}

// SignalFromAlert converts a firing alert into a Signal using this catalog.
// An overridden alert name maps even when the built-in table does not know
// it; the override severity is the default when the alert carries no
// severity label.
func (c *Catalog) SignalFromAlert(a Alert) (types.Signal, bool) {
	if c == nil || c.alerts == nil {
		return SignalFromAlert(a)
	}
	override, ok := c.alerts[a.Labels["alertname"]]
	if !ok {
		return SignalFromAlert(a)
	}
	if a.Status != "firing" {
		return types.Signal{}, false
	}
	sig, mapped := signalFromAlertWithClass(a, override.class, override.severity)
	return sig, mapped
}
