package chatgpt

import (
	"context"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// All OAuth traffic uses a synthetic transport; only the callback uses loopback.
func mockSignInTransport(t *testing.T, key *rsa.PrivateKey) transportFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("grant_type") != "authorization_code" {
				t.Error("unexpected grant type")
				return nil, errors.New("unexpected grant type")
			}
			id := r.Form.Get("client_id")
			cl := claims(r.Form.Get("code")) // The fake code carries the test nonce.
			cl["aud"] = id
			return response(tokenResponse{AccessToken: "authorized-" + id, RefreshToken: "authorized-refresh-" + id, IDToken: signed(t, key, cl), TokenType: "Bearer", ExpiresIn: 3600, Scope: Scope}), nil
		case jwksURL:
			return response(jwks(key)), nil
		case discoveryURL:
			return response(map[string]string{"issuer": Issuer, "revocation_endpoint": Issuer + "/revoke"}), nil
		case Issuer + "/revoke":
			return response(nil), nil
		default:
			t.Error("unexpected mock OAuth request")
			return nil, errors.New("unexpected request")
		}
	}
}

func beginMockSignIn(t *testing.T, c *Client, id string) (url.Values, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.SignIn(ctx, id, func(raw string) error { ready <- raw; return nil })
	}()
	select {
	case raw := <-ready:
		auth, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return auth.Query(), done
	case err := <-done:
		t.Fatalf("sign-in exited before browser wait: %v", err)
	case <-ctx.Done():
		t.Fatal("sign-in did not reach browser wait")
	}
	return nil, nil
}

func completeMockCallback(t *testing.T, auth url.Values, id string) {
	t.Helper()
	cb, err := url.Parse(auth.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	cb.RawQuery = url.Values{"state": {auth.Get("state")}, "code": {auth.Get("nonce")}, "client_id": {id}}.Encode()
	callbackGET(t, cb, http.StatusOK)
}

func awaitMockSignIn(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("sign-in did not finish")
	}
	return nil
}

func addMockAccount(t *testing.T, c *Client, id string) {
	t.Helper()
	if err := c.locked(context.Background(), func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		if err = c.save(credential{Account: Account{ClientID: id, Subject: "subject", ExpiresAt: time.Now().Add(time.Hour)}, AccessToken: "access-" + id, RefreshToken: "refresh-" + id, Scopes: Scope}); err != nil {
			return err
		}
		i.Clients = append(i.Clients, id)
		return c.saveIndex(i)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSignInLeavesAccountOperationsAvailable(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new registration"
		if existing {
			name = "saved registration"
		}
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			c.http.Transport = mockSignInTransport(t, signingKey(t))
			id := ""
			if existing {
				id = "account-b"
				addMockAccount(t, c, id)
			}
			auth, done := beginMockSignIn(t, c, id)
			// A separate client shares only the persisted store, as another CLI does.
			other := *c
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if token, err := other.Token(ctx); err != nil || token != "access" {
				t.Fatalf("pending sign-in blocked token access: %v", err)
			}
			if _, err := other.Status(ctx); err != nil {
				t.Fatalf("pending sign-in blocked status: %v", err)
			}
			if err := other.Select(ctx, c.selected); err != nil {
				t.Fatalf("pending sign-in blocked selection: %v", err)
			}
			if err := other.SignOut(ctx, c.selected); err != nil {
				t.Fatalf("pending sign-in blocked sign-out: %v", err)
			}
			completeMockCallback(t, auth, "account-b")
			if err := awaitMockSignIn(t, done); err != nil {
				t.Fatal(err)
			}
			i, err := c.loadIndex()
			if err != nil || i.Active != "" || len(i.Clients) != 2 {
				t.Fatalf("sign-in overwrote sign-out or lost registrations: %+v, %v", i, err)
			}
			v, err := c.load(c.selected)
			if err != nil || v.AccessToken != "" || v.RefreshToken != "" {
				t.Fatal("sign-in resurrected the signed-out account")
			}
			v, err = c.load("account-b")
			if err != nil || v.AccessToken != "authorized-account-b" {
				t.Fatal("independent sign-in did not persist")
			}
		})
	}
}

func TestPendingSignInPreservesExplicitReselection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.http.Transport = mockSignInTransport(t, signingKey(t))
	auth, done := beginMockSignIn(t, c, "")
	if err := c.Select(context.Background(), c.selected); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Fatalf("sign-in displaced a newer explicit re-selection: %+v, %v", i, err)
	}
}

func TestConcurrentSignInsMergeRegistrationsAndSelection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	c.http.Transport = mockSignInTransport(t, signingKey(t))
	first, firstDone := beginMockSignIn(t, c, "account-b")
	other := *c
	second, secondDone := beginMockSignIn(t, &other, "")
	completeMockCallback(t, second, "account-c")
	if err := awaitMockSignIn(t, secondDone); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, first, "account-b")
	if err := awaitMockSignIn(t, firstDone); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != "account-c" || len(i.Clients) != 3 || !slices.Contains(i.Clients, "account-b") || !slices.Contains(i.Clients, "account-c") {
		t.Fatalf("concurrent sign-in lost index updates: %+v, %v", i, err)
	}
}

func TestSignInRejectsConcurrentCredentialChange(t *testing.T) {
	for _, phase := range []string{"browser wait", "exchange"} {
		for _, mutation := range []string{"refresh", "sign-out"} {
			t.Run(phase+"/"+mutation, func(t *testing.T) {
				c := testClient(t)
				seed(t, c, time.Now().Add(-time.Minute))
				base := mockSignInTransport(t, signingKey(t))
				exchanging := make(chan struct{})
				release := make(chan struct{})
				var exchanges atomic.Int32
				c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() == tokenURL {
						_ = r.ParseForm()
						if r.Form.Get("grant_type") == "refresh_token" {
							return response(tokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: Scope}), nil
						}
						exchanges.Add(1)
						close(exchanging)
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					return base(r)
				})
				auth, done := beginMockSignIn(t, c, c.selected)
				if phase == "exchange" {
					completeMockCallback(t, auth, c.selected)
					select {
					case <-exchanging:
					case <-time.After(3 * time.Second):
						t.Fatal("exchange did not begin")
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if mutation == "refresh" {
					if token, err := c.Token(ctx); err != nil || token != "rotated-access" {
						t.Fatalf("concurrent refresh failed: %v", err)
					}
				} else if err := c.SignOut(ctx, c.selected); err != nil {
					t.Fatalf("concurrent sign-out failed: %v", err)
				}
				close(release)
				if phase == "browser wait" {
					completeMockCallback(t, auth, c.selected)
				}
				if err := awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "credentials changed") {
					t.Fatalf("stale sign-in was not rejected: %v", err)
				}
				if phase == "browser wait" && exchanges.Load() != 0 {
					t.Fatal("consumed code after credentials had already changed")
				}
				v, err := c.load(c.selected)
				if err != nil {
					t.Fatal(err)
				}
				if mutation == "refresh" {
					if v.AccessToken != "rotated-access" || v.RefreshToken != "rotated-refresh" {
						t.Fatal("stale OAuth response overwrote rotated credentials")
					}
				} else if v.AccessToken != "" || v.RefreshToken != "" || v.IDToken != "" {
					t.Fatal("stale OAuth response resurrected signed-out credentials")
				}
				// A fresh explicit invocation can still reauthorize the registration.
				c.http.Transport = base
				auth, done = beginMockSignIn(t, c, c.selected)
				completeMockCallback(t, auth, c.selected)
				if err = awaitMockSignIn(t, done); err != nil {
					t.Fatalf("fresh explicit sign-in failed: %v", err)
				}
			})
		}
	}
}

func TestOverlappingAuthorizationUsesIndependentLock(t *testing.T) {
	for _, newRegistration := range []bool{false, true} {
		name := "saved registration"
		if newRegistration {
			name = "new registration after callback"
		}
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			exchanging := make(chan struct{})
			release := make(chan struct{})
			base := mockSignInTransport(t, signingKey(t))
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == tokenURL {
					close(exchanging)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return base(r)
			})
			id := c.selected
			if newRegistration {
				id = ""
			}
			auth, done := beginMockSignIn(t, c, id)
			if newRegistration {
				id = "account-b"
				completeMockCallback(t, auth, id)
				select {
				case <-exchanging:
				case <-time.After(3 * time.Second):
					t.Fatal("exchange did not begin")
				}
			}
			other := *c
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			err := other.SignIn(ctx, id, func(string) error {
				t.Error("overlapping authorization reached browser")
				return errors.New("unexpected overlapping authorization")
			})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("overlapping authorization did not respect its lock: %v", err)
			}
			if _, err := other.Status(context.Background()); err != nil {
				t.Fatalf("authorization lock blocked store access: %v", err)
			}
			close(release)
			if !newRegistration {
				completeMockCallback(t, auth, id)
			}
			if err := awaitMockSignIn(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPendingRegistrationSignOutPreventsActivation(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	base := mockSignInTransport(t, signingKey(t))
	exchanging := make(chan struct{})
	release := make(chan struct{})
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == tokenURL {
			close(exchanging)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return base(r)
	})
	auth, done := beginMockSignIn(t, c, "")
	completeMockCallback(t, auth, "account-b")
	select {
	case <-exchanging:
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not begin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.SignOut(ctx, "account-b"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "credentials changed") {
		t.Fatalf("signed-out pending registration was activated: %v", err)
	}
	v, err := c.load("account-b")
	if err != nil || !v.PendingRegistration || v.AccessToken != "" || v.IDToken != "" {
		t.Fatal("pending registration was not left signed out")
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Fatal("pending registration displaced active account")
	}
}

func TestSignInRejectsChangedHost(t *testing.T) {
	for _, phase := range []string{"browser wait", "exchange"} {
		t.Run(phase, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			base := mockSignInTransport(t, signingKey(t))
			exchanging := make(chan struct{})
			release := make(chan struct{})
			var exchanges atomic.Int32
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == tokenURL {
					exchanges.Add(1)
					close(exchanging)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return base(r)
			})
			auth, done := beginMockSignIn(t, c, c.selected)
			if phase == "exchange" {
				completeMockCallback(t, auth, c.selected)
				select {
				case <-exchanging:
				case <-time.After(3 * time.Second):
					t.Fatal("exchange did not begin")
				}
			}
			if err := c.locked(context.Background(), func() error {
				i, err := c.loadIndex()
				if err != nil {
					return err
				}
				i.HostID = "different-host"
				return c.saveIndex(i)
			}); err != nil {
				t.Fatal(err)
			}
			close(release)
			if phase == "browser wait" {
				completeMockCallback(t, auth, c.selected)
			}
			if err := awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "host identity changed") {
				t.Fatalf("changed host was not rejected: %v", err)
			}
			if phase == "browser wait" && exchanges.Load() != 0 {
				t.Fatal("consumed code after host had already changed")
			}
			v, err := c.load(c.selected)
			if err != nil || v.AccessToken != "access" {
				t.Fatal("sign-in for old host changed credentials")
			}
		})
	}
}

func TestSignInRejectsLegacyCredentialWrites(t *testing.T) {
	for _, mutation := range []string{"refresh", "sign-out", "repeated sign-out", "preserved generation"} {
		t.Run(mutation, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			v, err := c.load(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Generation = ""
			if mutation == "repeated sign-out" {
				v.AccessToken, v.RefreshToken, v.Scopes = "", "", ""
			}
			if err = writeJSON(c.path(v.ClientID), v); err != nil {
				t.Fatal(err)
			}
			auth, done := beginMockSignIn(t, c, c.selected)
			if err = c.locked(context.Background(), func() error {
				current, err := c.load(c.selected)
				if err != nil {
					return err
				}
				if current.Generation == "" {
					t.Error("legacy sign-in snapshot was not upgraded")
				}
				if mutation != "preserved generation" {
					current.Generation = "" // Simulate an older writer dropping the field.
				}
				if mutation == "refresh" || mutation == "preserved generation" {
					current.AccessToken, current.RefreshToken = "legacy-access", "legacy-refresh"
				} else {
					current.AccessToken, current.RefreshToken, current.Scopes = "", "", ""
				}
				return writeJSON(c.path(current.ClientID), current)
			}); err != nil {
				t.Fatal(err)
			}
			completeMockCallback(t, auth, c.selected)
			if err = awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), "credentials changed") {
				t.Fatalf("legacy credential change was not detected before exchange: %v", err)
			}
		})
	}
}

func TestSignInTokenExpiryAnchoredToReceipt(t *testing.T) {
	c := testClient(t)
	received := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(received.Unix())
	c.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	base := mockSignInTransport(t, signingKey(t))
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			clock.Store(received.Add(2 * time.Minute).Unix())
		}
		return base(r)
	})
	auth, done := beginMockSignIn(t, c, "")
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	v, err := c.load("account-b")
	if err != nil || !v.ExpiresAt.Equal(received.Add(time.Hour)) {
		t.Fatalf("JWKS delay extended access-token expiry: %v, %v", v.ExpiresAt, err)
	}
}

func TestPendingSignInPreservesLegacyReselection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.http.Transport = mockSignInTransport(t, signingKey(t))
	auth, done := beginMockSignIn(t, c, "")
	if err := c.locked(context.Background(), func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		if i.SelectionVersion == 0 {
			t.Error("legacy selection snapshot was not upgraded")
		}
		// An older binary writes no version when explicitly reselecting A.
		i.SelectionVersion = 0
		return c.saveIndex(i)
	}); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Fatalf("sign-in displaced a legacy explicit re-selection: %+v, %v", i, err)
	}
}

func TestPendingSignInPreservesLegacyReselectionAcrossAnotherSignIn(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	addMockAccount(t, c, "account-c")
	c.http.Transport = mockSignInTransport(t, signingKey(t))
	if err := c.locked(context.Background(), func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		i.SelectionVersion = 1
		return c.saveIndex(i)
	}); err != nil {
		t.Fatal(err)
	}
	// B snapshots (A, 1), then an older binary explicitly reselects A and
	// drops the revision. C begins authorizing and initializes that revision.
	first, firstDone := beginMockSignIn(t, c, "account-b")
	if err := c.locked(context.Background(), func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		i.SelectionVersion = 0
		return c.saveIndex(i)
	}); err != nil {
		t.Fatal(err)
	}
	other := *c
	second, secondDone := beginMockSignIn(t, &other, "account-c")
	i, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if i.SelectionVersion == 0 || i.SelectionVersion == 1 {
		t.Error("normalizing a missing revision reused an earlier snapshot")
	}
	// C is still at the browser prompt when B completes. B must preserve A's
	// explicit selection, rather than mistaking C's initialization for (A, 1).
	completeMockCallback(t, first, "account-b")
	if err = awaitMockSignIn(t, firstDone); err != nil {
		t.Fatal(err)
	}
	i, err = c.loadIndex()
	if err != nil || i.Active != c.selected {
		t.Errorf("sign-in displaced a legacy explicit re-selection after revision normalization: %+v, %v", i, err)
	}
	completeMockCallback(t, second, "account-c")
	if err = awaitMockSignIn(t, secondDone); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSignInPreservesLegacyReselectionAcrossSelect(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	c.http.Transport = mockSignInTransport(t, signingKey(t))
	// Begin with a legacy index. Select used to create revision 1 here.
	if err := c.Select(context.Background(), c.selected); err != nil {
		t.Fatal(err)
	}
	before, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	auth, done := beginMockSignIn(t, c, "account-b")
	// An older Select(A) drops the revision. A newer Select(A) must not
	// recreate B's snapshot, even though the selected account stays the same.
	if err = c.locked(context.Background(), func() error {
		i, err := c.loadIndex()
		if err != nil {
			return err
		}
		i.SelectionVersion = 0
		return c.saveIndex(i)
	}); err != nil {
		t.Fatal(err)
	}
	other := *c
	if err = other.Select(context.Background(), c.selected); err != nil {
		t.Fatal(err)
	}
	after, err := c.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if after.SelectionVersion == 0 || after.SelectionVersion == before.SelectionVersion {
		t.Error("reselecting after a legacy reset recreated the pending snapshot")
	}
	completeMockCallback(t, auth, "account-b")
	if err = awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	after, err = c.loadIndex()
	if err != nil || after.Active != c.selected {
		t.Fatalf("pending sign-in overrode the newer Select(A): %+v, %v", after, err)
	}
}

func TestSelectionWritePathsUseFreshRevisions(t *testing.T) {
	for _, operation := range []string{"select", "active sign-out", "sign-in activation", "legacy initialization"} {
		for _, baseline := range []struct {
			name    string
			version uint64
		}{{"legacy", 0}, {"versioned", 37}} {
			if operation == "legacy initialization" && baseline.version != 0 {
				continue
			}
			t.Run(operation+"/"+baseline.name, func(t *testing.T) {
				c := testClient(t)
				seed(t, c, time.Now().Add(time.Hour))
				addMockAccount(t, c, "account-b")
				c.http.Transport = mockSignInTransport(t, signingKey(t))
				signedIn, err := c.load(c.selected)
				if err != nil {
					t.Fatal(err)
				}
				seen := make(map[uint64]bool)
				for range 2 {
					// Reset the same index revision, as a legacy writer can do.
					// Restore A's synthetic credentials for the sign-out case.
					if err = c.locked(context.Background(), func() error {
						i, err := c.loadIndex()
						if err != nil {
							return err
						}
						i.Active = c.selected
						i.SelectionVersion = baseline.version
						if err = c.save(signedIn); err != nil {
							return err
						}
						return c.saveIndex(i)
					}); err != nil {
						t.Fatal(err)
					}
					expectedActive := c.selected
					var activationSnapshot uint64
					switch operation {
					case "select":
						err = c.Select(context.Background(), c.selected)
					case "active sign-out":
						err = c.SignOut(context.Background(), c.selected)
						expectedActive = ""
					case "sign-in activation":
						auth, done := beginMockSignIn(t, c, "account-b")
						i, loadErr := c.loadIndex()
						if loadErr != nil {
							t.Fatal(loadErr)
						}
						activationSnapshot = i.SelectionVersion
						completeMockCallback(t, auth, "account-b")
						err = awaitMockSignIn(t, done)
						expectedActive = "account-b"
					case "legacy initialization":
						auth, done := beginMockSignIn(t, c, "account-b")
						cb, parseErr := url.Parse(auth.Get("redirect_uri"))
						if parseErr != nil {
							t.Fatal(parseErr)
						}
						// Decline after initialization to isolate its revision write.
						cb.RawQuery = url.Values{"state": {auth.Get("state")}, "error": {"access_denied"}}.Encode()
						callbackGET(t, cb, http.StatusOK)
						if declined := awaitMockSignIn(t, done); declined == nil || !strings.Contains(declined.Error(), "authorization was declined") {
							t.Fatalf("expected declined sign-in: %v", declined)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					i, err := c.loadIndex()
					if err != nil {
						t.Fatal(err)
					}
					if i.Active != expectedActive {
						t.Fatalf("wrong active selection: %q", i.Active)
					}
					if i.SelectionVersion == 0 || i.SelectionVersion == baseline.version || seen[i.SelectionVersion] {
						t.Fatal("selection write reused a revision after reset")
					}
					if operation == "sign-in activation" && i.SelectionVersion == activationSnapshot {
						t.Fatal("activation did not replace its authorization snapshot")
					}
					seen[i.SelectionVersion] = true
				}
			})
		}
	}
}
