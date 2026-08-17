package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

func criticalEvent(object string) ExternalEventRequest {
	return ExternalEventRequest{
		EventID: 331, Name: "Link Down", Severity: "Critical",
		ObjectName: object, OType: "IBPort", Description: "port went down",
	}
}

func TestRaiseEventPopulatesUFMShape(t *testing.T) {
	log := NewEventLog(100)
	event := log.Raise(criticalEvent("gpu-node-002"))

	if event.ID != 1 {
		t.Errorf("first event id = %d, want 1", event.ID)
	}
	if event.EventType != "331" {
		t.Errorf("event_type = %q, want \"331\" (string, as UFM reports it)", event.EventType)
	}
	if event.ObjectPath != "gpu-node-002(331)" {
		t.Errorf("object_path = %q", event.ObjectPath)
	}
	if event.Counter != "N/A" {
		t.Errorf("counter = %q, want N/A", event.Counter)
	}
	if _, err := time.Parse(TimeFormat, event.Timestamp); err != nil {
		t.Errorf("timestamp %q not in UFM format: %v", event.Timestamp, err)
	}
}

func TestSeverityDefaultsToInfo(t *testing.T) {
	log := NewEventLog(100)
	event := log.Raise(ExternalEventRequest{EventID: 1, Description: "x"})
	if event.Severity != "Info" {
		t.Errorf("severity = %q, want Info", event.Severity)
	}
	if _, alarms, _ := log.Counts(); alarms != 0 {
		t.Errorf("Info should not raise an alarm, got %d", alarms)
	}
}

func TestOnlyWarningAndAboveRaiseAlarms(t *testing.T) {
	for severity, wantAlarm := range map[string]bool{
		"Critical": true, "Error": true, "Warning": true,
		"Info": false, "Debug": false,
	} {
		log := NewEventLog(100)
		log.Raise(ExternalEventRequest{EventID: 1, Severity: severity, ObjectName: "n"})
		_, alarms, _ := log.Counts()
		if (alarms == 1) != wantAlarm {
			t.Errorf("severity %s: alarms=%d, wantAlarm=%v", severity, alarms, wantAlarm)
		}
	}
}

func TestRepeatedConditionBumpsCountNotAlarmCount(t *testing.T) {
	log := NewEventLog(100)
	log.Raise(criticalEvent("gpu-node-002"))
	log.Raise(criticalEvent("gpu-node-002"))
	log.Raise(criticalEvent("gpu-node-002"))

	alarms := log.Alarms("")
	if len(alarms) != 1 {
		t.Fatalf("got %d alarms, want 1 — repeats must not stack", len(alarms))
	}
	if alarms[0].EventCount != 3 {
		t.Errorf("event_count = %d, want 3", alarms[0].EventCount)
	}
	// The events themselves are still all recorded.
	if events, _, _ := log.Counts(); events != 3 {
		t.Errorf("events = %d, want 3", events)
	}
}

func TestDeleteAlarmsByDevice(t *testing.T) {
	log := NewEventLog(100)
	log.Raise(criticalEvent("node-a"))
	log.Raise(criticalEvent("node-b"))

	if removed := log.DeleteAlarms("node-a"); removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	remaining := log.Alarms("")
	if len(remaining) != 1 || remaining[0].ObjectName != "node-b" {
		t.Errorf("unexpected remaining alarms: %+v", remaining)
	}

	// Clearing with no device wipes the board.
	if removed := log.DeleteAlarms(""); removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if len(log.Alarms("")) != 0 {
		t.Error("all alarms should be cleared")
	}
	// Events are an audit trail and must survive.
	if events, _, _ := log.Counts(); events != 2 {
		t.Errorf("events = %d, want 2 after clearing alarms", events)
	}
}

func TestEventFilters(t *testing.T) {
	log := NewEventLog(100)
	log.Raise(criticalEvent("node-a"))
	log.Raise(ExternalEventRequest{
		EventID: 67, Severity: "Info", ObjectName: "node-b", Category: "Fabric Notification",
	})

	if got := log.Events(EventFilter{Severity: "critical"}); len(got) != 1 {
		t.Errorf("severity filter returned %d, want 1 (must be case-insensitive)", len(got))
	}
	if got := log.Events(EventFilter{ObjectName: "node-b"}); len(got) != 1 {
		t.Errorf("object_name filter returned %d, want 1", len(got))
	}
	if got := log.Events(EventFilter{}); len(got) != 2 {
		t.Errorf("empty filter returned %d, want 2", len(got))
	}
}

func TestSyslogLevelGating(t *testing.T) {
	log := NewEventLog(100)
	var streamed []Event
	log.SetEmitter(func(event Event) { streamed = append(streamed, event) })

	// Inactive by default: nothing streams.
	log.Raise(criticalEvent("node-a"))
	if len(streamed) != 0 {
		t.Fatal("nothing should stream while syslog is inactive")
	}

	log.SetSyslogConfig(SyslogConfig{
		Active: true, Destination: "127.0.0.1:5140", Level: "WARNING", EventsLog: true,
	})
	log.Raise(criticalEvent("node-a"))                                             // Critical: streams
	log.Raise(ExternalEventRequest{EventID: 2, Severity: "Info", ObjectName: "n"}) // Info: filtered

	if len(streamed) != 1 {
		t.Fatalf("streamed %d events, want 1 — Info is below WARNING", len(streamed))
	}
	if streamed[0].Severity != "Critical" {
		t.Errorf("streamed the wrong event: %+v", streamed[0])
	}

	// events_log=false disables event streaming even while active.
	log.SetSyslogConfig(SyslogConfig{
		Active: true, Destination: "127.0.0.1:5140", Level: "DEBUG", EventsLog: false,
	})
	log.Raise(criticalEvent("node-a"))
	if len(streamed) != 1 {
		t.Error("events_log=false must stop event streaming")
	}
}

func TestFormatEventIsRFC3164(t *testing.T) {
	event := Event{
		ID: 7, Severity: "Critical", Name: "Link Down", Description: "port down",
		ObjectName: "node-a", ObjectPath: "node-a(331)", Category: "Hardware",
	}
	message := FormatEvent("ufm-mock", event)

	// local0 (16) * 8 + critical (2) = 130
	if !strings.HasPrefix(message, "<130>") {
		t.Errorf("priority wrong, got %q", message[:12])
	}
	for _, want := range []string{"ufm-mock", "ufm:", "[7]", "CRITICAL", "port down", "node-a"} {
		if !strings.Contains(message, want) {
			t.Errorf("message missing %q: %s", want, message)
		}
	}
}

func TestForwardRoundTrip(t *testing.T) {
	record := map[string]string{"message": "hello ufm", "source": "ufm-efs"}
	frame := EncodeForward("ufm_syslog", time.Unix(1700000000, 0), record)

	tag, decoded, err := decodeForward(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tag != "ufm_syslog" {
		t.Errorf("tag = %q", tag)
	}
	if decoded["message"] != "hello ufm" || decoded["source"] != "ufm-efs" {
		t.Errorf("record = %+v", decoded)
	}
}

func TestForwardHandlesLongMessages(t *testing.T) {
	// Crosses the fixstr (32) and str8 (256) boundaries of the encoder.
	for _, size := range []int{10, 31, 32, 200, 255, 256, 5000} {
		message := strings.Repeat("x", size)
		frame := EncodeForward("t", time.Unix(1, 0), map[string]string{"message": message})
		_, decoded, err := decodeForward(bufio.NewReader(bytes.NewReader(frame)))
		if err != nil {
			t.Fatalf("size %d: decode: %v", size, err)
		}
		if decoded["message"] != message {
			t.Errorf("size %d: round trip corrupted the message", size)
		}
	}
}

func TestEFSConfigMergeIsPartial(t *testing.T) {
	efs := NewEFS()
	// The real plugin accepts a partial PUT and leaves everything else alone.
	config, err := efs.SetConfig([]byte(`{"streaming":{"enabled":true}}`))
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if !config.Streaming.Enabled {
		t.Error("streaming should be enabled")
	}
	if config.FluentBitEndpoint.MessageTagName != "ufm_syslog" {
		t.Errorf("partial PUT clobbered message_tag_name: %q",
			config.FluentBitEndpoint.MessageTagName)
	}
	if config.UFMSyslogEndpoint.Port != 5140 {
		t.Errorf("partial PUT clobbered the syslog port: %d", config.UFMSyslogEndpoint.Port)
	}
}

func TestEventLogRetentionCap(t *testing.T) {
	log := NewEventLog(5)
	for i := 0; i < 20; i++ {
		log.Raise(ExternalEventRequest{EventID: i, Severity: "Info", ObjectName: "n"})
	}
	if events, _, _ := log.Counts(); events != 5 {
		t.Errorf("retained %d events, want the 5 most recent", events)
	}
	// The survivors must be the newest ones.
	kept := log.Events(EventFilter{})
	if kept[len(kept)-1].EventType != "19" {
		t.Errorf("newest retained event_type = %q, want 19", kept[len(kept)-1].EventType)
	}
}
