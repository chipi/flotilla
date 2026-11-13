package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHostEnrolment(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	issued := 0
	issue := func(serial string) func() (string, time.Time, error) {
		return func() (string, time.Time, error) { issued++; return serial, clock.Add(30 * 24 * time.Hour), nil }
	}
	if _, err := s.AddEnrolment(ctx, "mini", "tok-1", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HostCertCurrent(ctx, "mini", ""); ok {
		t.Fatal("an unenrolled host's empty serial is current")
	}
	// another host's token, an unknown one: refused, and nothing is issued
	for name, c := range map[string][2]string{"another host": {"dgx", "tok-1"}, "an unknown token": {"mini", "tok-x"}} {
		if err := s.Enrol(ctx, c[0], c[1], issue("s0")); !errors.Is(err, ErrEnrolment) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a failed issue does not spend the token
	if err := s.Enrol(ctx, "mini", "tok-1", func() (string, time.Time, error) { return "", time.Time{}, errors.New("bad request") }); err == nil {
		t.Fatal("a failed issue enrolled")
	}
	if err := s.Enrol(ctx, "mini", "tok-1", issue("s1")); err != nil {
		t.Fatal(err)
	}
	if issued != 1 {
		t.Fatalf("issued %d times", issued)
	}
	if err := s.Enrol(ctx, "mini", "tok-1", issue("s2")); !errors.Is(err, ErrEnrolment) || issued != 1 {
		t.Fatalf("a token used twice: %v, issued %d", err, issued)
	}
	if ok, _ := s.HostCertCurrent(ctx, "mini", "s1"); !ok {
		t.Fatal("the enrolled certificate is not current")
	}
	// a late token
	if _, err := s.AddEnrolment(ctx, "mini", "tok-2", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(EnrolmentValidity)
	if err := s.Enrol(ctx, "mini", "tok-2", issue("s2")); !errors.Is(err, ErrEnrolment) || issued != 1 {
		t.Fatalf("a token at its expiry: %v", err)
	}
	clock = clock.Add(-time.Second)
	if err := s.Enrol(ctx, "mini", "tok-2", issue("s2")); err != nil {
		t.Fatalf("a token a second before its expiry: %v", err)
	}
	// re-enrolment replaces the certificate: only the newest is current
	if ok, _ := s.HostCertCurrent(ctx, "mini", "s1"); ok {
		t.Fatal("the replaced certificate is still current")
	}
	if ok, _ := s.HostCertCurrent(ctx, "mini", "s2"); !ok {
		t.Fatal("the new certificate is not current")
	}
	// revoke: the certificate and any unused token stop working; a new token brings the host back
	if _, err := s.AddEnrolment(ctx, "mini", "tok-3", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeHost(ctx, "mini", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HostCertCurrent(ctx, "mini", "s2"); ok {
		t.Fatal("a revoked host's certificate is current")
	}
	if err := s.Enrol(ctx, "mini", "tok-3", issue("s3")); !errors.Is(err, ErrEnrolment) {
		t.Fatalf("a token minted before the revocation: %v", err)
	}
	if err := s.RevokeHost(ctx, "dgx", "user:ada", "api"); !errors.Is(err, ErrNoSuchHost) {
		t.Fatalf("revoking an unknown host: %v", err)
	}
	if _, err := s.AddEnrolment(ctx, "mini", "tok-4", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enrol(ctx, "mini", "tok-4", issue("s4")); err != nil {
		t.Fatal(err)
	}
	hs, err := s.Hosts(ctx)
	if err != nil || len(hs) != 1 || hs[0].CertSerial != "s4" || hs[0].RevokedAt != nil || hs[0].EnrolledAt == nil {
		t.Fatalf("hosts: %+v %v", hs, err)
	}
	var kinds []string
	rows, _ := s.db.Query(`SELECT kind FROM events WHERE subject = 'host:mini' ORDER BY seq`)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	want := "host.enrolment host.enrolled host.enrolment host.enrolled host.enrolment host.revoked host.enrolment host.enrolled"
	if got := join(kinds); got != want {
		t.Fatalf("events:\n%s\nwant:\n%s", got, want)
	}
}
