package chatgpt

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var uuidHostPattern = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestHostIDUUIDVersionVariantAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for range 32 {
		id, err := newHostID()
		if err != nil {
			t.Fatal(err)
		}
		if !uuidHostPattern.MatchString(id) {
			t.Fatalf("invalid UUIDv4 host format: %q", id)
		}
		if seen[id] {
			t.Fatal("host ID repeated")
		}
		seen[id] = true
	}
}

func TestHostIDFreshAndLegacyInitialization(t *testing.T) {
	for _, initial := range []string{"", "roast-" + strings.Repeat("ab", 32)} {
		for _, entrypoint := range []string{"prepare-host", "signin"} {
			t.Run(entrypoint+initial, func(t *testing.T) {
				c := testClient(t)
				if err := c.saveIndex(index{HostID: initial}); err != nil {
					t.Fatal(err)
				}
				var id string
				if entrypoint == "prepare-host" {
					var err error
					id, _, err = c.PrepareHost(context.Background())
					if err != nil {
						t.Fatal(err)
					}
				} else {
					stop := errors.New("stop before browser authorization")
					err := c.SignIn(context.Background(), "", func(raw string) error { u, _ := url.Parse(raw); id = u.Query().Get("ext_agent_host_id"); return stop })
					if !errors.Is(err, stop) {
						t.Fatalf("signin did not reach URL presentation: %v", err)
					}
				}
				if !uuidHostPattern.MatchString(id) {
					t.Fatalf("unsupported host format: %q", id)
				}
				saved, err := c.loadIndex()
				if err != nil || saved.HostID != id || len(saved.Clients) != 0 || saved.Active != "" {
					t.Fatal("host initialization changed registration state")
				}
				again, _, err := c.PrepareHost(context.Background())
				if err != nil || again != id {
					t.Fatal("host identity was not stable")
				}
			})
		}
	}
}

func TestLegacyHostNeverMigratesWithRegistrations(t *testing.T) {
	for _, state := range []string{"active", "indexed", "orphaned"} {
		for _, entrypoint := range []string{"prepare-host", "signin"} {
			t.Run(state+entrypoint, func(t *testing.T) {
				c := testClient(t)
				legacy := "roast-" + strings.Repeat("ab", 32)
				i := index{HostID: legacy}
				switch state {
				case "active":
					i.Active = "issued-client"
				case "indexed":
					i.Clients = []string{"issued-client"}
				case "orphaned":
					if err := c.save(credential{Account: Account{ClientID: "issued-client", Subject: "verified-subject"}}); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.saveIndex(i); err != nil {
					t.Fatal(err)
				}
				var err error
				if entrypoint == "prepare-host" {
					_, _, err = c.PrepareHost(context.Background())
				} else {
					err = c.SignIn(context.Background(), "", func(string) error { t.Error("must not start authorization while host identity is invalid"); return nil })
				}
				if err == nil || !strings.Contains(err.Error(), "refusing to replace") {
					t.Fatalf("registered legacy host was not protected: %v", err)
				}
				saved, e := c.loadIndex()
				if e != nil || saved.HostID != legacy || saved.Active != i.Active || len(saved.Clients) != len(i.Clients) {
					t.Fatal("registered host identity changed")
				}
			})
		}
	}
}

func TestNonLegacyHostIdentityRetained(t *testing.T) {
	for _, id := range []string{"urn:uuid:00000000-0000-4000-8000-000000000001", "urn:ietf:params:oauth:jwk-thumbprint:sha-256:example", "did:key:example", "roast-not-a-generated-legacy-id"} {
		c := testClient(t)
		if err := c.saveIndex(index{HostID: id}); err != nil {
			t.Fatal(err)
		}
		got, _, err := c.PrepareHost(context.Background())
		if err != nil || got != id {
			t.Fatal("existing identity was silently replaced")
		}
	}
}
