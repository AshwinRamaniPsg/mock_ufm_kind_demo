package main

import (
	"encoding/json"
	"testing"
)

// seedFabric returns a fabric holding two active ports from one source.
func seedFabric(t *testing.T) *Fabric {
	t.Helper()
	fabric := NewFabric("6.18.0")
	outcome, err := fabric.Reconcile(InventorySnapshot{
		InventoryID: "inventory-a",
		EpochID:     "epoch-a",
		Generation:  1,
		Machines: []InventoryMachine{{
			MatID:     "mat-a",
			MachineID: "machine-a",
			InfinibandPorts: []InventoryPort{
				{GUID: "0x1", State: "active"},
				{GUID: "0x2", State: "down"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}
	if outcome != "applied" {
		t.Fatalf("seed outcome = %q, want applied", outcome)
	}
	return fabric
}

func TestGUIDAndPKeyFormatting(t *testing.T) {
	for _, input := range []string{"0x1", "1", "0000000000000001"} {
		guid, err := ParseGUID(input)
		if err != nil {
			t.Fatalf("ParseGUID(%q): %v", input, err)
		}
		if got := FormatGUID(guid); got != "0000000000000001" {
			t.Errorf("FormatGUID(%q) = %q, want 0000000000000001", input, got)
		}
	}

	if _, err := ParsePKey("0x8000"); err == nil {
		t.Error("ParsePKey(0x8000) should fail: above the default pkey")
	}
	if key, _ := ParsePKey("0x7fff"); FormatPKey(key) != "0x7fff" {
		t.Error("default pkey should round-trip as 0x7fff")
	}
	// A bare number is decimal, matching the UFM API.
	if key, _ := ParsePKey("16"); key != 16 {
		t.Errorf("ParsePKey(16) = %d, want 16", key)
	}
}

func TestPortsReportLinkState(t *testing.T) {
	ports := seedFabric(t).Ports()
	if len(ports) != 2 {
		t.Fatalf("got %d ports, want 2", len(ports))
	}
	if ports[0].GUID != "0000000000000001" || ports[0].SystemID != "0000000000000001" {
		t.Errorf("unexpected first port: %+v", ports[0])
	}
	if ports[0].Name != "0000000000000001_1" {
		t.Errorf("port name = %q, want <guid>_1", ports[0].Name)
	}
	if ports[0].PhysicalState != "Link Up" || ports[0].LogicalState != "Active" {
		t.Errorf("active port states = %q/%q", ports[0].PhysicalState, ports[0].LogicalState)
	}
	if ports[1].PhysicalState != "Link Down" || ports[1].LogicalState != "Down" {
		t.Errorf("down port states = %q/%q", ports[1].PhysicalState, ports[1].LogicalState)
	}
	if ports[0].SystemName != "machine-a" || ports[0].DName != "mat-a" {
		t.Errorf("naming = %q/%q", ports[0].SystemName, ports[0].DName)
	}
}

func TestDefaultPartitionShape(t *testing.T) {
	partitions := seedFabric(t).Partitions(true, true)
	management, ok := partitions["0x7fff"]
	if !ok {
		t.Fatal("default partition 0x7fff missing")
	}
	if management.Partition != "management" || !management.IPOverIB {
		t.Errorf("unexpected default partition: %+v", management)
	}
	if management.Membership != "limited" {
		t.Errorf("default membership = %q, want limited", management.Membership)
	}

	// An empty membership list must serialise as [], not disappear.
	encoded, err := json.Marshal(management)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	guids, ok := decoded["guids"].([]any)
	if !ok || len(guids) != 0 {
		t.Errorf("guids = %#v, want empty array", decoded["guids"])
	}
}

func TestNonDefaultPartitionOmitsMembership(t *testing.T) {
	fabric := seedFabric(t)
	if err := fabric.Bind(BindRequest{
		PKey: "1", IPOverIB: true, Membership: "full", Index0: true, GUIDs: []string{"0x1"},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	data, ok := fabric.Partition(1, true, true)
	if !ok {
		t.Fatal("partition 0x1 missing after bind")
	}
	if data.Partition != "api_pkey_0x1" {
		t.Errorf("name = %q, want api_pkey_0x1", data.Partition)
	}
	if data.Membership != "" {
		t.Errorf("non-default partition should omit membership, got %q", data.Membership)
	}
	if data.QoSConf == nil || data.QoSConf.RateLimit != 2.5 {
		t.Errorf("new partition should start at the default QoS, got %+v", data.QoSConf)
	}
	if data.GUIDs == nil || len(*data.GUIDs) != 1 || (*data.GUIDs)[0].Membership != "full" {
		t.Errorf("unexpected members: %+v", data.GUIDs)
	}
}

func TestBindRejectsUnknownGUIDAtomically(t *testing.T) {
	fabric := seedFabric(t)
	err := fabric.Bind(BindRequest{
		PKey: "2", Membership: "full", GUIDs: []string{"0x1", "0xdeadbeef"},
	})
	if err == nil {
		t.Fatal("bind with an unknown GUID should fail")
	}
	if fabricErr, ok := err.(*FabricError); !ok || fabricErr.Status != 404 {
		t.Fatalf("want 404 FabricError, got %#v", err)
	}
	// The known GUID must not have been partially bound.
	if _, ok := fabric.Partition(2, false, false); ok {
		t.Error("partition 0x2 should not exist after a rejected bind")
	}
}

func TestUnbindDeletesEmptiedPartition(t *testing.T) {
	fabric := seedFabric(t)
	if err := fabric.Bind(BindRequest{PKey: "1", Membership: "full", GUIDs: []string{"0x1"}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := fabric.Unbind(UnbindRequest{PKey: "1", GUIDs: []string{"0x1"}}); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if _, ok := fabric.Partition(1, false, false); ok {
		t.Error("emptied non-default partition should be deleted")
	}
	// The default partition survives even when empty.
	if _, ok := fabric.Partition(DefaultPKey, false, false); !ok {
		t.Error("default partition must always exist")
	}
}

func TestUpdateQoSRequiresExistingPartition(t *testing.T) {
	fabric := seedFabric(t)
	err := fabric.UpdateQoS(QoSRequest{PKey: "0x33", MTULimit: 4, ServiceLevel: 3, RateLimit: 100})
	if fabricErr, ok := err.(*FabricError); !ok || fabricErr.Status != 404 {
		t.Fatalf("want 404 for a missing partition, got %#v", err)
	}

	if err := fabric.Bind(BindRequest{PKey: "1", Membership: "full", GUIDs: []string{"0x1"}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := fabric.UpdateQoS(QoSRequest{PKey: "1", MTULimit: 4, ServiceLevel: 3, RateLimit: 100}); err != nil {
		t.Fatalf("update qos: %v", err)
	}
	data, _ := fabric.Partition(1, false, true)
	if data.QoSConf.MTULimit != 4 || data.QoSConf.RateLimit != 100 {
		t.Errorf("qos not applied: %+v", data.QoSConf)
	}
}

func TestReconcileGenerationWatermark(t *testing.T) {
	fabric := seedFabric(t)
	snapshot := InventorySnapshot{
		InventoryID: "inventory-a", EpochID: "epoch-a", Generation: 1,
		Machines: []InventoryMachine{{MatID: "mat-a"}},
	}

	if outcome, _ := fabric.Reconcile(snapshot); outcome != "duplicate" {
		t.Errorf("replaying generation 1 = %q, want duplicate", outcome)
	}
	snapshot.Generation = 0
	if outcome, _ := fabric.Reconcile(snapshot); outcome != "stale" {
		t.Errorf("older generation = %q, want stale", outcome)
	}
	// Both were ignored, so the seeded ports survive.
	if ports, _, _, _ := fabric.Stats(); ports != 2 {
		t.Errorf("ports = %d, want 2 after ignored snapshots", ports)
	}

	// A new epoch means the source restarted: the watermark resets.
	snapshot.EpochID = "epoch-b"
	snapshot.Generation = 1
	if outcome, _ := fabric.Reconcile(snapshot); outcome != "applied" {
		t.Error("a new epoch should reset the generation watermark")
	}
}

func TestReconcileRemovesVanishedPortsAndPartitions(t *testing.T) {
	fabric := seedFabric(t)
	if err := fabric.Bind(BindRequest{PKey: "1", Membership: "full", GUIDs: []string{"0x2"}}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Generation 2 no longer advertises 0x2.
	if _, err := fabric.Reconcile(InventorySnapshot{
		InventoryID: "inventory-a", EpochID: "epoch-a", Generation: 2,
		Machines: []InventoryMachine{{
			MatID:           "mat-a",
			MachineID:       "machine-a",
			InfinibandPorts: []InventoryPort{{GUID: "0x1", State: "active"}},
		}},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if ports, _, _, _ := fabric.Stats(); ports != 1 {
		t.Errorf("ports = %d, want 1 after 0x2 vanished", ports)
	}
	if _, ok := fabric.Partition(1, false, false); ok {
		t.Error("partition emptied by reconciliation should be deleted")
	}
}

func TestReconcileKeepsLIDAndMembershipStable(t *testing.T) {
	fabric := seedFabric(t)
	if err := fabric.Bind(BindRequest{PKey: "1", Membership: "full", GUIDs: []string{"0x1"}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	before := fabric.Ports()[0].LID

	// Re-advertise the same GUID with a changed link state.
	if _, err := fabric.Reconcile(InventorySnapshot{
		InventoryID: "inventory-a", EpochID: "epoch-a", Generation: 2,
		Machines: []InventoryMachine{{
			MatID:     "mat-a",
			MachineID: "machine-a",
			InfinibandPorts: []InventoryPort{
				{GUID: "0x1", State: "down"},
				{GUID: "0x2", State: "down"},
			},
		}},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if after := fabric.Ports()[0].LID; after != before {
		t.Errorf("LID changed across reconcile: %d -> %d", before, after)
	}
	if fabric.Ports()[0].LogicalState != "Down" {
		t.Error("link state should have been updated to Down")
	}
	data, ok := fabric.Partition(1, true, false)
	if !ok || data.GUIDs == nil || len(*data.GUIDs) != 1 {
		t.Error("pkey membership should survive reconciliation of an unchanged GUID")
	}
}

func TestReconcileRejectsEmptyIdentity(t *testing.T) {
	fabric := NewFabric("6.18.0")
	_, err := fabric.Reconcile(InventorySnapshot{InventoryID: "", EpochID: "epoch-a"})
	if fabricErr, ok := err.(*FabricError); !ok || fabricErr.Status != 400 {
		t.Fatalf("want 400 for an empty inventory_id, got %#v", err)
	}
}
