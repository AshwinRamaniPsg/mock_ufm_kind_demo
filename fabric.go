package main

// Fabric state for the mock UFM.
//
// Response shapes, partition naming, LID assignment and reconciliation
// semantics follow NVIDIA's own Rust mock in
// github.com/NVIDIA/infra-controller, crates/ufm-mock (Apache-2.0):
// src/state/{types,ports,partitions,reconciliation}.rs.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultPKey is the management partition every UFM fabric always has.
const DefaultPKey uint16 = 0x7fff

// ---------------------------------------------------------------------------
// GUIDs and partition keys
// ---------------------------------------------------------------------------

// ParseGUID accepts "0x1", "1" or "0000000000000001".
func ParseGUID(value string) (uint64, error) {
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "0x"), "0X")
	guid, err := strconv.ParseUint(text, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid GUID %q", value)
	}
	return guid, nil
}

// FormatGUID renders a GUID the way UFM reports it: 16 lowercase hex digits.
func FormatGUID(guid uint64) string { return fmt.Sprintf("%016x", guid) }

// ParsePKey accepts "0x7fff" (hex) or "1" (decimal), matching the UFM API.
func ParsePKey(value string) (uint16, error) {
	text := strings.TrimSpace(value)
	var parsed uint64
	var err error
	if lower := strings.ToLower(text); strings.HasPrefix(lower, "0x") {
		parsed, err = strconv.ParseUint(lower[2:], 16, 64)
	} else {
		parsed, err = strconv.ParseUint(text, 10, 64)
	}
	if err != nil || parsed > uint64(DefaultPKey) {
		return 0, fmt.Errorf("invalid partition key %q", value)
	}
	return uint16(parsed), nil
}

// FormatPKey renders a pkey as UFM does: "0x7fff".
func FormatPKey(key uint16) string { return fmt.Sprintf("0x%x", key) }

// ---------------------------------------------------------------------------
// wire types
// ---------------------------------------------------------------------------

// PortData is one entry of GET /resources/ports.
type PortData struct {
	GUID          string `json:"guid"`
	Name          string `json:"name"`
	SystemID      string `json:"systemID"`
	LID           int    `json:"lid"`
	DName         string `json:"dname"`
	SystemName    string `json:"system_name"`
	PhysicalState string `json:"physical_state"`
	LogicalState  string `json:"logical_state"`
}

// QoS carries the per-partition QoS configuration.
type QoS struct {
	MTULimit     int     `json:"mtu_limit"`
	ServiceLevel int     `json:"service_level"`
	RateLimit    float64 `json:"rate_limit"`
}

// DefaultQoS matches the values UFM applies to a freshly created partition.
func DefaultQoS() QoS { return QoS{MTULimit: 2, ServiceLevel: 0, RateLimit: 2.5} }

// MemberData is one GUID inside a partition, returned when guids_data=true.
type MemberData struct {
	GUID       string `json:"guid"`
	Membership string `json:"membership"`
	Index0     bool   `json:"index0"`
}

// PartitionData is the JSON shape of a pkey. Optional fields are omitted
// unless the caller asked for them via guids_data / qos_conf.
// GUIDs is a pointer so a partition with no members still serialises as
// "guids": [] when guids_data=true, rather than being omitted.
type PartitionData struct {
	Partition  string        `json:"partition"`
	IPOverIB   bool          `json:"ip_over_ib"`
	QoSConf    *QoS          `json:"qos_conf,omitempty"`
	GUIDs      *[]MemberData `json:"guids,omitempty"`
	Membership string        `json:"membership,omitempty"`
}

// SMConfig is the subnet-manager view returned by GET /app/smconf.
type SMConfig struct {
	SubnetPrefix string `json:"subnet_prefix"`
	MKey         string `json:"m_key"`
	SMKey        string `json:"sm_key"`
	SAKey        string `json:"sa_key"`
	MKeyPerPort  bool   `json:"m_key_per_port"`
}

// BindRequest is the body of POST /resources/pkeys.
type BindRequest struct {
	PKey       string   `json:"pkey"`
	IPOverIB   bool     `json:"ip_over_ib"`
	Membership string   `json:"membership"`
	Index0     bool     `json:"index0"`
	GUIDs      []string `json:"guids"`
}

// UnbindRequest is the body of POST /actions/remove_guids_from_pkey.
type UnbindRequest struct {
	PKey  string   `json:"pkey"`
	GUIDs []string `json:"guids"`
}

// QoSRequest is the body of PUT /resources/pkeys/qos_conf.
type QoSRequest struct {
	PKey         string  `json:"pkey"`
	MTULimit     int     `json:"mtu_limit"`
	ServiceLevel int     `json:"service_level"`
	RateLimit    float64 `json:"rate_limit"`
}

// InventorySnapshot is a complete, versioned view of one inventory source.
type InventorySnapshot struct {
	InventoryID string             `json:"inventory_id"`
	EpochID     string             `json:"epoch_id"`
	Generation  int64              `json:"generation"`
	Machines    []InventoryMachine `json:"machines"`
}

// InventoryMachine is one machine and the IB ports it advertises.
type InventoryMachine struct {
	MatID           string          `json:"mat_id"`
	MachineID       string          `json:"machine_id,omitempty"`
	InfinibandPorts []InventoryPort `json:"infiniband_ports,omitempty"`
}

// InventoryPort is a single IB port GUID and its simulated link state.
type InventoryPort struct {
	GUID  string `json:"guid"`
	State string `json:"state"` // "active" or "down"
}

// ---------------------------------------------------------------------------
// internal state
// ---------------------------------------------------------------------------

type portRecord struct {
	matID     string
	machineID string
	state     string
	lid       int
	owner     string
}

type memberConfig struct {
	membership string
	index0     bool
}

type partition struct {
	name     string
	ipOverIB bool
	qos      QoS
	members  map[uint64]memberConfig
}

type sourceState struct {
	epochID    string
	generation int64
	guids      map[uint64]struct{}
}

// Fabric is the in-memory model of one simulated InfiniBand fabric.
type Fabric struct {
	mu         sync.RWMutex
	ports      map[uint64]*portRecord
	partitions map[uint16]*partition
	sources    map[string]*sourceState
	nextLID    int

	Version  string
	SMConfig SMConfig
}

// NewFabric returns a fabric containing only the default management partition.
func NewFabric(version string) *Fabric {
	return &Fabric{
		ports: map[uint64]*portRecord{},
		partitions: map[uint16]*partition{
			DefaultPKey: {
				name:     "management",
				ipOverIB: true,
				qos:      DefaultQoS(),
				members:  map[uint64]memberConfig{},
			},
		},
		sources: map[string]*sourceState{},
		nextLID: 1,
		Version: version,
		SMConfig: SMConfig{
			SubnetPrefix: "0xfe80000000000000",
			MKey:         "0x0000000000000000",
			SMKey:        "0x0000000000000001",
			SAKey:        "0x0000000000000001",
		},
	}
}

// ---------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------

// FabricError carries the HTTP status UFM would return for a failure.
type FabricError struct {
	Message string
	Status  int
}

func (e *FabricError) Error() string { return e.Message }

func notFound(format string, args ...any) *FabricError {
	return &FabricError{Message: fmt.Sprintf(format, args...), Status: 404}
}

func badRequest(format string, args ...any) *FabricError {
	return &FabricError{Message: fmt.Sprintf(format, args...), Status: 400}
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

// Ports returns every port in the fabric, ordered by GUID.
func (f *Fabric) Ports() []PortData {
	f.mu.RLock()
	defer f.mu.RUnlock()

	guids := make([]uint64, 0, len(f.ports))
	for guid := range f.ports {
		guids = append(guids, guid)
	}
	sort.Slice(guids, func(i, j int) bool { return guids[i] < guids[j] })

	out := make([]PortData, 0, len(guids))
	for _, guid := range guids {
		record := f.ports[guid]
		text := FormatGUID(guid)
		active := record.state == "active"
		systemName := record.machineID
		if systemName == "" {
			systemName = record.matID
		}
		physical, logical := "Link Down", "Down"
		if active {
			physical, logical = "Link Up", "Active"
		}
		out = append(out, PortData{
			GUID:          text,
			Name:          text + "_1",
			SystemID:      text,
			LID:           record.lid,
			DName:         record.matID,
			SystemName:    systemName,
			PhysicalState: physical,
			LogicalState:  logical,
		})
	}
	return out
}

// partitionData renders one partition. Caller holds at least a read lock.
func partitionData(key uint16, part *partition, includeGUIDs, includeQoS bool) PartitionData {
	data := PartitionData{Partition: part.name, IPOverIB: part.ipOverIB}
	if includeQoS {
		qos := part.qos
		data.QoSConf = &qos
	}
	if includeGUIDs {
		guids := make([]uint64, 0, len(part.members))
		for guid := range part.members {
			guids = append(guids, guid)
		}
		sort.Slice(guids, func(i, j int) bool { return guids[i] < guids[j] })
		members := make([]MemberData, 0, len(guids))
		for _, guid := range guids {
			config := part.members[guid]
			members = append(members, MemberData{
				GUID:       FormatGUID(guid),
				Membership: config.membership,
				Index0:     config.index0,
			})
		}
		data.GUIDs = &members
	}
	// Only the default partition reports a top-level membership.
	if key == DefaultPKey {
		data.Membership = "limited"
	}
	return data
}

// Partitions returns every pkey keyed by its "0x..." representation.
func (f *Fabric) Partitions(includeGUIDs, includeQoS bool) map[string]PartitionData {
	f.mu.RLock()
	defer f.mu.RUnlock()

	out := make(map[string]PartitionData, len(f.partitions))
	for key, part := range f.partitions {
		out[FormatPKey(key)] = partitionData(key, part, includeGUIDs, includeQoS)
	}
	return out
}

// Partition returns one pkey, or ok=false when it does not exist. UFM answers
// 200 with an empty object in that case, so the caller decides the encoding.
func (f *Fabric) Partition(key uint16, includeGUIDs, includeQoS bool) (PartitionData, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	part, ok := f.partitions[key]
	if !ok {
		return PartitionData{}, false
	}
	return partitionData(key, part, includeGUIDs, includeQoS), true
}

// Stats reports counters for the metrics endpoint.
func (f *Fabric) Stats() (ports, active, partitions, sources int) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	for _, record := range f.ports {
		if record.state == "active" {
			active++
		}
	}
	return len(f.ports), active, len(f.partitions), len(f.sources)
}

// ---------------------------------------------------------------------------
// writes
// ---------------------------------------------------------------------------

// Bind adds GUIDs to a pkey, creating the partition when it does not exist.
func (f *Fabric) Bind(request BindRequest) error {
	key, err := ParsePKey(request.PKey)
	if err != nil {
		return badRequest("%s", err)
	}
	guids := make([]uint64, 0, len(request.GUIDs))
	for _, text := range request.GUIDs {
		guid, err := ParseGUID(text)
		if err != nil {
			return badRequest("%s", err)
		}
		guids = append(guids, guid)
	}
	membership := request.Membership
	if membership == "" {
		membership = "full"
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Reject the whole request if any GUID is unknown, as UFM does.
	for _, guid := range guids {
		if _, ok := f.ports[guid]; !ok {
			return notFound("InfiniBand port %s does not exist", FormatGUID(guid))
		}
	}

	part, ok := f.partitions[key]
	if !ok {
		part = &partition{
			name:    "api_pkey_" + FormatPKey(key),
			qos:     DefaultQoS(),
			members: map[uint64]memberConfig{},
		}
		f.partitions[key] = part
	}
	part.ipOverIB = request.IPOverIB
	for _, guid := range guids {
		part.members[guid] = memberConfig{membership: membership, index0: request.Index0}
	}
	return nil
}

// Unbind removes GUIDs from a pkey. Emptying a non-default partition deletes it.
func (f *Fabric) Unbind(request UnbindRequest) error {
	key, err := ParsePKey(request.PKey)
	if err != nil {
		return badRequest("%s", err)
	}
	guids := make([]uint64, 0, len(request.GUIDs))
	for _, text := range request.GUIDs {
		guid, err := ParseGUID(text)
		if err != nil {
			return badRequest("%s", err)
		}
		guids = append(guids, guid)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	part, ok := f.partitions[key]
	if !ok {
		return nil // unbinding an unknown pkey is a no-op in UFM
	}
	for _, guid := range guids {
		delete(part.members, guid)
	}
	if key != DefaultPKey && len(part.members) == 0 {
		delete(f.partitions, key)
	}
	return nil
}

// UpdateQoS replaces the QoS configuration of an existing partition.
func (f *Fabric) UpdateQoS(request QoSRequest) error {
	key, err := ParsePKey(request.PKey)
	if err != nil {
		return badRequest("%s", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	part, ok := f.partitions[key]
	if !ok {
		return notFound("partition %s does not exist", FormatPKey(key))
	}
	part.qos = QoS{
		MTULimit:     request.MTULimit,
		ServiceLevel: request.ServiceLevel,
		RateLimit:    request.RateLimit,
	}
	return nil
}

// ---------------------------------------------------------------------------
// inventory reconciliation
// ---------------------------------------------------------------------------

// Reconcile applies a complete inventory snapshot and reports what happened:
// "applied", "duplicate" or "stale".
//
// A snapshot is authoritative for its own inventory_id: GUIDs it stops
// reporting are removed from the fabric and from every partition. Generations
// only move forward inside one epoch; a new epoch (a source restart) resets
// the watermark.
func (f *Fabric) Reconcile(snapshot InventorySnapshot) (string, error) {
	if snapshot.InventoryID == "" || snapshot.EpochID == "" {
		return "", badRequest("inventory_id and epoch_id must not be empty")
	}

	seen := map[uint64]*portRecord{}
	for _, machine := range snapshot.Machines {
		for _, port := range machine.InfinibandPorts {
			guid, err := ParseGUID(port.GUID)
			if err != nil {
				return "", badRequest("%s", err)
			}
			state := strings.ToLower(port.State)
			if state == "" {
				state = "active"
			}
			seen[guid] = &portRecord{
				matID:     machine.MatID,
				machineID: machine.MachineID,
				state:     state,
				owner:     snapshot.InventoryID,
			}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	source, known := f.sources[snapshot.InventoryID]
	if known && source.epochID == snapshot.EpochID {
		if snapshot.Generation == source.generation {
			return "duplicate", nil
		}
		if snapshot.Generation < source.generation {
			return "stale", nil
		}
	}

	for guid, candidate := range seen {
		if existing, ok := f.ports[guid]; ok {
			// Keep the LID stable so pkey membership survives an update.
			existing.matID = candidate.matID
			existing.machineID = candidate.machineID
			existing.state = candidate.state
			existing.owner = candidate.owner
			continue
		}
		candidate.lid = f.nextLID
		f.nextLID++
		f.ports[guid] = candidate
	}

	// Drop GUIDs this source used to report but no longer does.
	removed := false
	if known {
		for guid := range source.guids {
			if _, still := seen[guid]; still {
				continue
			}
			if record, ok := f.ports[guid]; ok && record.owner == snapshot.InventoryID {
				delete(f.ports, guid)
				for _, part := range f.partitions {
					delete(part.members, guid)
				}
				removed = true
			}
		}
	}
	// A non-default partition left with no members disappears, the same as
	// when its last GUID is explicitly unbound.
	if removed {
		for key, part := range f.partitions {
			if key != DefaultPKey && len(part.members) == 0 {
				delete(f.partitions, key)
			}
		}
	}

	guids := make(map[uint64]struct{}, len(seen))
	for guid := range seen {
		guids[guid] = struct{}{}
	}
	f.sources[snapshot.InventoryID] = &sourceState{
		epochID:    snapshot.EpochID,
		generation: snapshot.Generation,
		guids:      guids,
	}
	return "applied", nil
}
