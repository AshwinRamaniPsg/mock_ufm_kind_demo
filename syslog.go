package main

// RFC 3164 syslog emission.
//
// UFM, when `PUT /app/syslog` sets active=true, writes its events to a syslog
// destination. The EFS plugin's whole job is to listen on that destination and
// forward what arrives. Emitting real datagrams here is what makes the EFS
// path testable end to end.

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// syslogFacility is local0, the facility UFM logs under.
const syslogFacility = 16

// SyslogSender writes datagrams to a UDP destination, reconnecting when the
// destination changes.
type SyslogSender struct {
	mu       sync.Mutex
	conn     net.Conn
	dest     string
	hostname string
}

// NewSyslogSender returns a sender that tags messages with hostname.
func NewSyslogSender(hostname string) *SyslogSender {
	if hostname == "" {
		hostname = "ufm-mock"
	}
	return &SyslogSender{hostname: hostname}
}

// FormatEvent renders an event as an RFC 3164 message. Exported for tests.
func FormatEvent(hostname string, event Event) string {
	severity, ok := severityToSyslog[strings.ToLower(event.Severity)]
	if !ok {
		severity = 6
	}
	priority := syslogFacility*8 + severity
	stamp := time.Now().Format("Jan  2 15:04:05")

	// UFM writes a single line per event; EFS forwards it verbatim.
	return fmt.Sprintf("<%d>%s %s ufm: [%d] %s %s: %s [object=%s path=%s category=%s]",
		priority, stamp, hostname,
		event.ID, strings.ToUpper(event.Severity), event.Name, event.Description,
		event.ObjectName, event.ObjectPath, event.Category)
}

// Send delivers one event to destination ("host:port"). A send failure is
// reported but never fatal — a missing collector must not break the fabric API.
func (s *SyslogSender) Send(destination string, event Event) error {
	if destination == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn == nil || s.dest != destination {
		if s.conn != nil {
			_ = s.conn.Close()
		}
		conn, err := net.DialTimeout("udp", destination, 3*time.Second)
		if err != nil {
			s.conn = nil
			return fmt.Errorf("dial syslog %s: %w", destination, err)
		}
		s.conn = conn
		s.dest = destination
	}

	if _, err := s.conn.Write([]byte(FormatEvent(s.hostname, event))); err != nil {
		// Drop the connection so the next send redials.
		_ = s.conn.Close()
		s.conn = nil
		return fmt.Errorf("write syslog: %w", err)
	}
	return nil
}

// Close releases the socket.
func (s *SyslogSender) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}
