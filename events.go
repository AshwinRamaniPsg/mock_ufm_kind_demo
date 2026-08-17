package main

// Events, alarms and UFM syslog configuration.
//
// Shapes follow the UFM Enterprise REST API guide:
//   Events REST API  -> /app/events, /app/events/{id}, /app/events/external_event(s)
//   Alarms REST API  -> /app/alarms, /app/alarms/{id}, DELETE ?device_id=
//   Syslog config    -> /app/syslog
//
// In real UFM an alarm is the standing, unacknowledged condition behind an
// event. Here, any event at Warning or worse also raises an alarm, which is
// what makes "raise an alarm / clear an alarm" demonstrable.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// TimeFormat is how UFM renders timestamps in event and alarm objects.
const TimeFormat = "2006-01-02 15:04:05"

// Event is one entry of GET /app/events.
type Event struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	EventType     string `json:"event_type"`
	Severity      string `json:"severity"`
	Timestamp     string `json:"timestamp"`
	Counter       string `json:"counter"`
	Category      string `json:"category"`
	ObjectName    string `json:"object_name"`
	ObjectPath    string `json:"object_path"`
	WriteToSyslog bool   `json:"write_to_syslog"`
	Description   string `json:"description"`
}

// Alarm is one entry of GET /app/alarms.
type Alarm struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	EventType   string `json:"event_type"`
	Duration    string `json:"duration"`
	Reason      string `json:"reason"`
	Severity    string `json:"severity"`
	Timestamp   string `json:"timestamp"`
	Counter     string `json:"counter"`
	EventCount  int    `json:"event_count"`
	ObjectName  string `json:"object_name"`
	ObjectPath  string `json:"object_path"`

	raised time.Time
}

// ExternalEventRequest is the body of POST /app/events/external_event.
// event_id, description and object_name mirror the real UFM API; the rest are
// optional refinements this mock accepts.
type ExternalEventRequest struct {
	EventID     int    `json:"event_id"`
	Description string `json:"description"`
	ObjectName  string `json:"object_name"`
	OType       string `json:"otype"`
	Severity    string `json:"severity"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Counter     string `json:"counter"`
}

// SyslogConfig is the body of PUT /app/syslog. When active, the mock emits
// each event as an RFC 3164 syslog datagram to destination — which is exactly
// what the EFS plugin listens for.
type SyslogConfig struct {
	Active      bool   `json:"active"`
	Destination string `json:"destination"`
	Level       string `json:"level"`
	UFMLog      bool   `json:"ufm_log"`
	EventsLog   bool   `json:"events_log"`
}

// severityRank orders severities so a syslog level filter can be applied.
// Lower is more severe, matching syslog itself.
var severityRank = map[string]int{
	"critical": 0,
	"error":    1,
	"warning":  2,
	"info":     3,
	"debug":    4,
}

// severityToSyslog maps a UFM severity onto an RFC 3164 severity code.
var severityToSyslog = map[string]int{
	"critical": 2,
	"error":    3,
	"warning":  4,
	"info":     6,
	"debug":    7,
}

// NormalizeSeverity canonicalises a severity to UFM's capitalisation,
// defaulting to Info.
func NormalizeSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical":
		return "Critical"
	case "error":
		return "Error"
	case "warning":
		return "Warning"
	case "debug":
		return "Debug"
	case "info", "":
		return "Info"
	default:
		return "Info"
	}
}

// AlarmWorthy reports whether a severity raises a standing alarm.
func AlarmWorthy(severity string) bool {
	return severityRank[strings.ToLower(severity)] <= severityRank["warning"]
}

// EventLog holds events, alarms and the syslog configuration.
type EventLog struct {
	mu       sync.RWMutex
	events   []Event
	alarms   map[int64]*Alarm
	nextID   int64
	maxEvent int
	syslog   SyslogConfig

	// emit is called for every event that passes the syslog filter. It is a
	// field so the syslog sender can be swapped out in tests.
	emit func(Event)
}

// NewEventLog returns an empty log retaining at most maxEvents events.
func NewEventLog(maxEvents int) *EventLog {
	if maxEvents <= 0 {
		maxEvents = 1000
	}
	return &EventLog{
		alarms:   map[int64]*Alarm{},
		nextID:   1,
		maxEvent: maxEvents,
		syslog:   SyslogConfig{Level: "INFO"},
	}
}

// SetEmitter installs the syslog sender.
func (l *EventLog) SetEmitter(emit func(Event)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit = emit
}

// SyslogConfig returns the current syslog configuration.
func (l *EventLog) SyslogConfig() SyslogConfig {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.syslog
}

// SetSyslogConfig replaces the syslog configuration.
func (l *EventLog) SetSyslogConfig(config SyslogConfig) SyslogConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	if config.Level == "" {
		config.Level = "INFO"
	}
	l.syslog = config
	return l.syslog
}

// shouldForward reports whether an event passes the configured syslog filter.
// Caller holds at least a read lock.
func (l *EventLog) shouldForward(event Event) bool {
	if !l.syslog.Active || l.syslog.Destination == "" || !l.syslog.EventsLog {
		return false
	}
	threshold, ok := severityRank[strings.ToLower(l.syslog.Level)]
	if !ok {
		threshold = severityRank["info"]
	}
	return severityRank[strings.ToLower(event.Severity)] <= threshold
}

// Raise records an event, raises an alarm when severe enough, and returns the
// stored event.
func (l *EventLog) Raise(request ExternalEventRequest) Event {
	severity := NormalizeSeverity(request.Severity)
	now := time.Now()

	name := request.Name
	if name == "" {
		name = "External Event"
	}
	objectType := request.OType
	if objectType == "" {
		objectType = "Device"
	}
	category := request.Category
	if category == "" {
		category = "Fabric Notification"
	}
	counter := request.Counter
	if counter == "" {
		counter = "N/A"
	}
	objectName := request.ObjectName
	if objectName == "" {
		objectName = "default"
	}

	l.mu.Lock()
	id := l.nextID
	l.nextID++

	event := Event{
		ID:            id,
		Name:          name,
		Type:          objectType,
		EventType:     fmt.Sprintf("%d", request.EventID),
		Severity:      severity,
		Timestamp:     now.Format(TimeFormat),
		Counter:       counter,
		Category:      category,
		ObjectName:    objectName,
		ObjectPath:    fmt.Sprintf("%s(%d)", objectName, request.EventID),
		WriteToSyslog: l.syslog.Active && l.syslog.EventsLog,
		Description:   request.Description,
	}
	l.events = append(l.events, event)
	if len(l.events) > l.maxEvent {
		l.events = l.events[len(l.events)-l.maxEvent:]
	}

	// A repeat condition on the same object bumps the existing alarm rather
	// than stacking a new one, as UFM does.
	if AlarmWorthy(severity) {
		if existing := l.findAlarm(objectName, event.EventType); existing != nil {
			existing.EventCount++
			existing.Timestamp = event.Timestamp
			existing.Severity = severity
		} else {
			l.alarms[id] = &Alarm{
				ID:          id,
				Type:        objectType,
				Name:        name,
				Description: request.Description,
				EventType:   event.EventType,
				Duration:    "0:00:00",
				Reason:      request.Description,
				Severity:    severity,
				Timestamp:   event.Timestamp,
				Counter:     counter,
				EventCount:  1,
				ObjectName:  objectName,
				ObjectPath:  event.ObjectPath,
				raised:      now,
			}
		}
	}

	forward := l.shouldForward(event)
	emit := l.emit
	l.mu.Unlock()

	if forward && emit != nil {
		emit(event)
	}
	return event
}

// findAlarm returns an existing alarm for the object. Caller holds the lock.
func (l *EventLog) findAlarm(objectName, eventType string) *Alarm {
	for _, alarm := range l.alarms {
		if alarm.ObjectName == objectName && alarm.EventType == eventType {
			return alarm
		}
	}
	return nil
}

// EventFilter narrows a GET /app/events listing.
type EventFilter struct {
	ObjectName string
	Type       string
	Category   string
	Severity   string
	Group      string
}

// Events returns matching events, newest last.
func (l *EventLog) Events(filter EventFilter) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()

	match := func(want, got string) bool {
		return want == "" || strings.EqualFold(want, got)
	}
	out := make([]Event, 0, len(l.events))
	for _, event := range l.events {
		if match(filter.ObjectName, event.ObjectName) &&
			match(filter.Type, event.Type) &&
			match(filter.Category, event.Category) &&
			match(filter.Severity, event.Severity) {
			out = append(out, event)
		}
	}
	return out
}

// Event returns one event by ID.
func (l *EventLog) Event(id int64) (Event, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, event := range l.events {
		if event.ID == id {
			return event, true
		}
	}
	return Event{}, false
}

// Alarms returns active alarms, optionally limited to one device, ordered by
// ID. Duration is computed at read time.
func (l *EventLog) Alarms(deviceID string) []Alarm {
	l.mu.RLock()
	defer l.mu.RUnlock()

	ids := make([]int64, 0, len(l.alarms))
	for id := range l.alarms {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]Alarm, 0, len(ids))
	for _, id := range ids {
		alarm := l.alarms[id]
		if deviceID != "" && !strings.EqualFold(alarm.ObjectName, deviceID) {
			continue
		}
		copied := *alarm
		elapsed := time.Since(alarm.raised).Round(time.Second)
		copied.Duration = fmt.Sprintf("%d:%02d:%02d",
			int(elapsed.Hours()), int(elapsed.Minutes())%60, int(elapsed.Seconds())%60)
		out = append(out, copied)
	}
	return out
}

// Alarm returns one alarm by ID.
func (l *EventLog) Alarm(id int64) (Alarm, bool) {
	for _, alarm := range l.Alarms("") {
		if alarm.ID == id {
			return alarm, true
		}
	}
	return Alarm{}, false
}

// DeleteAlarms clears alarms for a device, or all alarms when deviceID is
// empty. It returns how many were removed.
func (l *EventLog) DeleteAlarms(deviceID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	removed := 0
	for id, alarm := range l.alarms {
		if deviceID == "" || strings.EqualFold(alarm.ObjectName, deviceID) {
			delete(l.alarms, id)
			removed++
		}
	}
	return removed
}

// DeleteAlarm clears one alarm by ID.
func (l *EventLog) DeleteAlarm(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.alarms[id]; !ok {
		return false
	}
	delete(l.alarms, id)
	return true
}

// Counts reports event and alarm totals for the metrics endpoint.
func (l *EventLog) Counts() (events, alarms, critical int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, alarm := range l.alarms {
		if strings.EqualFold(alarm.Severity, "Critical") {
			critical++
		}
	}
	return len(l.events), len(l.alarms), critical
}
