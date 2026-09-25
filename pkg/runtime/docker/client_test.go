package docker

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	meta "github.com/weaveworks/ignite/pkg/apis/meta/v1alpha1"
)

func TestPortBindingsToDocker(t *testing.T) {
	mappings := meta.PortMappings{
		{BindAddress: net.ParseIP("127.0.0.1"), HostPort: 8080, VMPort: 80, Protocol: meta.ProtocolTCP},
		{HostPort: 5353, VMPort: 53, Protocol: meta.ProtocolUDP},
		{HostPort: 2222, VMPort: 22},
	}

	bindings, exposed, err := portBindingsToDocker(mappings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bindings) != 3 || len(exposed) != 3 {
		t.Fatalf("expected 3 bindings and 3 exposed ports, got %d and %d", len(bindings), len(exposed))
	}

	// The wire format must be identical to the go-connections/nat form the
	// v20.10 client sent: {"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}],...}
	got, err := json.Marshal(bindings)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string][]map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"80/tcp": {"127.0.0.1", "8080"},
		"53/udp": {"", "5353"},
		"22/tcp": {"", "2222"},
	}
	for port, hp := range want {
		b, ok := decoded[port]
		if !ok || len(b) != 1 {
			t.Fatalf("missing binding for %s in %s", port, got)
		}
		if b[0]["HostIp"] != hp[0] || b[0]["HostPort"] != hp[1] {
			t.Errorf("binding %s = %v, want HostIp=%q HostPort=%q", port, b[0], hp[0], hp[1])
		}
	}
}

func TestDurationToSeconds(t *testing.T) {
	if durationToSeconds(nil) != nil {
		t.Fatal("nil timeout must stay nil so the engine default applies")
	}
	for d, want := range map[time.Duration]int{
		10 * time.Second:        10,
		1500 * time.Millisecond: 2,
		0:                       0,
	} {
		d := d
		if got := durationToSeconds(&d); got == nil || *got != want {
			t.Errorf("durationToSeconds(%v) = %v, want %d", d, got, want)
		}
	}
}

func TestLegacyNetworkSettingsIPAddress(t *testing.T) {
	raw := []byte(`{"Id":"abc","NetworkSettings":{"IPAddress":"172.17.0.5","Networks":{"bridge":{"IPAddress":"172.17.0.5"}}}}`)
	if got := legacyNetworkSettingsIPAddress(raw); got != "172.17.0.5" {
		t.Fatalf("got %q", got)
	}
	for _, raw := range [][]byte{nil, []byte(`{}`), []byte(`not json`), []byte(`{"NetworkSettings":null}`)} {
		if got := legacyNetworkSettingsIPAddress(raw); got != "" {
			t.Errorf("legacyNetworkSettingsIPAddress(%s) = %q, want empty", raw, got)
		}
	}
}
