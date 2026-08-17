package main

// EFS — UFM Events Fluent Streaming.
//
// The real plugin (Mellanox/ufm_sdk_3.0, plugins/ufm_syslog_streaming_plugin,
// image mellanox/ufm-plugin-efs) is a container that:
//
//   1. listens for UFM's syslog datagrams on [UFM-syslog-endpoint],
//   2. forwards them to a Fluentd/Fluent Bit destination using the Fluent
//      Forward protocol, tagged [fluent-bit-endpoint.message_tag_name],
//   3. optionally duplicates them to a remote syslog server,
//   4. is configured over REST at GET/PUT /plugin/efs/conf.
//
// This is a Go stand-in with the same config schema and the same wire
// behaviour. The real image is amd64-only and last published in 2024, so it
// will not run on an arm64 kind node — this will.
//
// It also provides a `sink` mode: a Fluent Forward receiver that prints what
// it gets, so the pipeline can be demonstrated without pulling Fluentd.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// EFSConfig mirrors the plugin's config file section for section.
type EFSConfig struct {
	UFMSyslogEndpoint struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"UFM-syslog-endpoint"`

	FluentBitEndpoint struct {
		Enabled         bool   `json:"enabled"`
		SourcePort      int    `json:"source_port"`
		DestinationHost string `json:"destination_host"`
		DestinationPort int    `json:"destination_port"`
		MessageTagName  string `json:"message_tag_name"`
	} `json:"fluent-bit-endpoint"`

	SyslogDestinationEndpoint struct {
		Enabled bool   `json:"enabled"`
		Host    string `json:"host"`
		Port    int    `json:"port"`
	} `json:"syslog-destination-endpoint"`

	Streaming struct {
		Enabled bool `json:"enabled"`
	} `json:"streaming"`

	LogsConfig struct {
		LogsFileName       string `json:"logs_file_name"`
		LogsLevel          string `json:"logs_level"`
		LogFileMaxSize     int    `json:"log_file_max_size"`
		LogFileBackupCount int    `json:"log_file_backup_count"`
	} `json:"logs-config"`
}

// DefaultEFSConfig matches conf/ufm_syslog_streaming_plugin.cfg upstream.
func DefaultEFSConfig() EFSConfig {
	var config EFSConfig
	config.UFMSyslogEndpoint.Host = "0.0.0.0"
	config.UFMSyslogEndpoint.Port = 5140
	config.FluentBitEndpoint.SourcePort = 24227
	config.FluentBitEndpoint.DestinationHost = "127.0.0.1"
	config.FluentBitEndpoint.DestinationPort = 24225
	config.FluentBitEndpoint.MessageTagName = "ufm_syslog"
	config.SyslogDestinationEndpoint.Host = "127.0.0.1"
	config.SyslogDestinationEndpoint.Port = 514
	config.LogsConfig.LogsFileName = "/log/efs.log"
	config.LogsConfig.LogsLevel = "INFO"
	config.LogsConfig.LogFileMaxSize = 10485760
	config.LogsConfig.LogFileBackupCount = 5
	return config
}

// EFSStats counts what the forwarder has handled.
type EFSStats struct {
	Received  int64 `json:"received"`
	Forwarded int64 `json:"forwarded"`
	Syslog    int64 `json:"syslog_forwarded"`
	Errors    int64 `json:"errors"`
}

// EFS is the forwarder: one UDP listener plus two optional outputs.
type EFS struct {
	mu      sync.RWMutex
	config  EFSConfig
	stats   EFSStats
	recent  []string
	conn    *net.UDPConn
	restart chan struct{}
}

// NewEFS returns a forwarder with the upstream default configuration.
func NewEFS() *EFS {
	return &EFS{config: DefaultEFSConfig(), restart: make(chan struct{}, 1)}
}

// Config returns the current configuration.
func (e *EFS) Config() EFSConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.config
}

// Stats returns counters and the most recent messages seen.
func (e *EFS) Stats() (EFSStats, []string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	recent := make([]string, len(e.recent))
	copy(recent, e.recent)
	return e.stats, recent
}

// SetConfig merges a partial configuration, exactly like the real plugin: a
// PUT carrying only {"streaming":{"enabled":true}} leaves everything else
// alone. Updating while running restarts the listener.
func (e *EFS) SetConfig(raw []byte) (EFSConfig, error) {
	e.mu.Lock()
	merged := e.config
	// Unmarshalling onto a copy of the current config gives us the merge.
	if err := json.Unmarshal(raw, &merged); err != nil {
		e.mu.Unlock()
		return EFSConfig{}, fmt.Errorf("invalid EFS configuration: %w", err)
	}
	e.config = merged
	e.mu.Unlock()

	select {
	case e.restart <- struct{}{}:
	default:
	}
	return merged, nil
}

// Run listens for syslog datagrams and forwards them until ctx is cancelled.
// It restarts the listener whenever the configuration changes.
func (e *EFS) Run(done <-chan struct{}) {
	for {
		config := e.Config()
		if !config.Streaming.Enabled {
			log.Printf("streaming disabled — set it with PUT /plugin/efs/conf")
		} else {
			e.listen(config, done)
		}

		select {
		case <-done:
			return
		case <-e.restart:
			log.Printf("configuration changed, restarting forwarder")
		}
	}
}

// listen serves one configuration generation.
func (e *EFS) listen(config EFSConfig, done <-chan struct{}) {
	address := fmt.Sprintf("%s:%d", config.UFMSyslogEndpoint.Host, config.UFMSyslogEndpoint.Port)
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		log.Printf("bad UFM syslog endpoint %s: %v", address, err)
		return
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Printf("cannot listen on %s: %v", address, err)
		return
	}

	e.mu.Lock()
	e.conn = conn
	e.mu.Unlock()

	log.Printf("listening for UFM syslog on %s", address)
	if config.FluentBitEndpoint.Enabled {
		log.Printf("forwarding to Fluentd %s:%d tag=%s",
			config.FluentBitEndpoint.DestinationHost,
			config.FluentBitEndpoint.DestinationPort,
			config.FluentBitEndpoint.MessageTagName)
	}
	if config.SyslogDestinationEndpoint.Enabled {
		log.Printf("duplicating to syslog %s:%d",
			config.SyslogDestinationEndpoint.Host, config.SyslogDestinationEndpoint.Port)
	}

	// Close the socket when the config changes or we shut down, which
	// unblocks ReadFromUDP below.
	stop := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-e.restart:
			// Put it back so Run sees it too.
			select {
			case e.restart <- struct{}{}:
			default:
			}
		case <-stop:
			return
		}
		_ = conn.Close()
	}()
	defer close(stop)

	buffer := make([]byte, 64*1024)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return // socket closed by the watcher above
		}
		e.handle(config, string(buffer[:n]))
	}
}

// handle records a message and fans it out to the configured destinations.
func (e *EFS) handle(config EFSConfig, message string) {
	e.mu.Lock()
	e.stats.Received++
	e.recent = append(e.recent, message)
	if len(e.recent) > 50 {
		e.recent = e.recent[len(e.recent)-50:]
	}
	e.mu.Unlock()

	log.Printf("recv: %s", message)

	if config.FluentBitEndpoint.Enabled {
		target := fmt.Sprintf("%s:%d",
			config.FluentBitEndpoint.DestinationHost, config.FluentBitEndpoint.DestinationPort)
		if err := forwardToFluentd(target, config.FluentBitEndpoint.MessageTagName, message); err != nil {
			log.Printf("fluentd forward failed: %v", err)
			e.mu.Lock()
			e.stats.Errors++
			e.mu.Unlock()
		} else {
			e.mu.Lock()
			e.stats.Forwarded++
			e.mu.Unlock()
		}
	}

	if config.SyslogDestinationEndpoint.Enabled {
		target := fmt.Sprintf("%s:%d",
			config.SyslogDestinationEndpoint.Host, config.SyslogDestinationEndpoint.Port)
		if err := forwardToSyslog(target, message); err != nil {
			log.Printf("syslog forward failed: %v", err)
			e.mu.Lock()
			e.stats.Errors++
			e.mu.Unlock()
		} else {
			e.mu.Lock()
			e.stats.Syslog++
			e.mu.Unlock()
		}
	}
}

func forwardToSyslog(target, message string) error {
	conn, err := net.DialTimeout("udp", target, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(message))
	return err
}

// ---------------------------------------------------------------------------
// Fluent Forward protocol
// ---------------------------------------------------------------------------
//
// Fluentd's forward input speaks MessagePack. A Message-mode entry is:
//
//     [ tag, time, record ]
//
// Only four MessagePack types are needed — string, uint, map and array — so
// the encoder below is written by hand rather than pulling in a dependency.

func msgpackString(out []byte, value string) []byte {
	length := len(value)
	switch {
	case length < 32:
		out = append(out, byte(0xa0|length)) // fixstr
	case length < 256:
		out = append(out, 0xd9, byte(length)) // str8
	case length < 65536:
		out = append(out, 0xda, byte(length>>8), byte(length)) // str16
	default:
		out = append(out, 0xdb,
			byte(length>>24), byte(length>>16), byte(length>>8), byte(length)) // str32
	}
	return append(out, value...)
}

func msgpackUint(out []byte, value uint64) []byte {
	switch {
	case value < 128:
		return append(out, byte(value)) // positive fixint
	case value < 256:
		return append(out, 0xcc, byte(value))
	case value < 65536:
		return append(out, 0xcd, byte(value>>8), byte(value))
	case value < 1<<32:
		return append(out, 0xce,
			byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	default:
		return append(out, 0xcf,
			byte(value>>56), byte(value>>48), byte(value>>40), byte(value>>32),
			byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	}
}

// EncodeForward builds a Fluent Forward Message-mode frame. Exported for tests.
func EncodeForward(tag string, timestamp time.Time, record map[string]string) []byte {
	out := []byte{0x93} // fixarray of 3: tag, time, record
	out = msgpackString(out, tag)
	out = msgpackUint(out, uint64(timestamp.Unix()))

	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	// Deterministic ordering keeps the wire bytes reproducible in tests.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	if len(keys) < 16 {
		out = append(out, byte(0x80|len(keys))) // fixmap
	} else {
		out = append(out, 0xde, byte(len(keys)>>8), byte(len(keys))) // map16
	}
	for _, key := range keys {
		out = msgpackString(out, key)
		out = msgpackString(out, record[key])
	}
	return out
}

func forwardToFluentd(target, tag, message string) error {
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	record := map[string]string{"message": message, "source": "ufm-efs"}
	frame := EncodeForward(tag, time.Now(), record)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write(frame)
	return err
}

// ---------------------------------------------------------------------------
// REST surface
// ---------------------------------------------------------------------------

// Handler serves the plugin's REST API. The real plugin listens on 8989 and
// UFM proxies it at /ufmRest/plugin/efs/...; both paths are accepted here.
func (e *EFS) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSuffix(r.URL.Path, "/")
		path = strings.TrimPrefix(path, "/ufmRest/plugin/efs")
		path = strings.TrimPrefix(path, "/plugin/efs")
		if path == "" {
			path = "/"
		}

		writeJSON := func(status int, payload any) {
			body, _ := json.Marshal(payload)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}

		switch {
		case path == "/conf" && r.Method == http.MethodGet:
			writeJSON(http.StatusOK, e.Config())

		case path == "/conf" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
			raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				writeJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			config, err := e.SetConfig(raw)
			if err != nil {
				writeJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(http.StatusOK, config)

		case path == "/stats":
			stats, recent := e.Stats()
			writeJSON(http.StatusOK, map[string]any{"stats": stats, "recent": recent})

		case path == "/health" || path == "/":
			stats, _ := e.Stats()
			writeJSON(http.StatusOK, map[string]any{
				"status":    "ok",
				"streaming": e.Config().Streaming.Enabled,
				"received":  stats.Received,
			})

		default:
			writeJSON(http.StatusNotFound, map[string]string{"error": "no route for " + path})
		}
	})
}

// ---------------------------------------------------------------------------
// sink mode
// ---------------------------------------------------------------------------

// RunSink accepts Fluent Forward connections and prints each record, standing
// in for Fluentd so the pipeline can be shown without pulling another image.
func RunSink(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen %s: %w", address, err)
	}
	log.Printf("Fluentd-compatible sink listening on %s", address)

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go func(conn net.Conn) {
			defer conn.Close()
			reader := bufio.NewReader(conn)
			for {
				tag, record, err := decodeForward(reader)
				if err != nil {
					if err != io.EOF {
						log.Printf("decode error: %v", err)
					}
					return
				}
				log.Printf("[%s] %s", tag, record["message"])
			}
		}(conn)
	}
}

// decodeForward reads one Message-mode frame. It understands only the subset
// this project's encoder emits, plus what Fluent Bit sends.
func decodeForward(reader *bufio.Reader) (string, map[string]string, error) {
	header, err := reader.ReadByte()
	if err != nil {
		return "", nil, err
	}
	if header != 0x93 && header != 0x92 {
		return "", nil, fmt.Errorf("unexpected frame header 0x%02x", header)
	}

	tag, err := decodeString(reader)
	if err != nil {
		return "", nil, err
	}
	if err := skipValue(reader); err != nil { // timestamp
		return "", nil, err
	}

	mapHeader, err := reader.ReadByte()
	if err != nil {
		return "", nil, err
	}
	var count int
	switch {
	case mapHeader&0xf0 == 0x80:
		count = int(mapHeader & 0x0f)
	case mapHeader == 0xde:
		high, _ := reader.ReadByte()
		low, err := reader.ReadByte()
		if err != nil {
			return "", nil, err
		}
		count = int(high)<<8 | int(low)
	default:
		return "", nil, fmt.Errorf("unexpected map header 0x%02x", mapHeader)
	}

	record := make(map[string]string, count)
	for i := 0; i < count; i++ {
		key, err := decodeString(reader)
		if err != nil {
			return "", nil, err
		}
		value, err := decodeString(reader)
		if err != nil {
			return "", nil, err
		}
		record[key] = value
	}
	return tag, record, nil
}

func decodeString(reader *bufio.Reader) (string, error) {
	header, err := reader.ReadByte()
	if err != nil {
		return "", err
	}
	var length int
	switch {
	case header&0xe0 == 0xa0:
		length = int(header & 0x1f)
	case header == 0xd9:
		size, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		length = int(size)
	case header == 0xda:
		high, _ := reader.ReadByte()
		low, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		length = int(high)<<8 | int(low)
	case header == 0xc4: // bin8, which Fluent Bit may use for values
		size, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		length = int(size)
	default:
		return "", fmt.Errorf("unexpected string header 0x%02x", header)
	}
	buffer := make([]byte, length)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return "", err
	}
	return string(buffer), nil
}

// skipValue consumes one value, enough to step over a timestamp.
func skipValue(reader *bufio.Reader) error {
	header, err := reader.ReadByte()
	if err != nil {
		return err
	}
	switch {
	case header < 0x80: // positive fixint
		return nil
	case header == 0xcc:
		_, err = reader.Discard(1)
	case header == 0xcd:
		_, err = reader.Discard(2)
	case header == 0xce:
		_, err = reader.Discard(4)
	case header == 0xcf:
		_, err = reader.Discard(8)
	case header == 0xd7: // EventTime extension
		_, err = reader.Discard(9)
	default:
		return fmt.Errorf("unexpected timestamp header 0x%02x", header)
	}
	return err
}
