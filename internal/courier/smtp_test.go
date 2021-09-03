package courier

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"blitiri.com.ar/go/chasquid/internal/domaininfo"
	"blitiri.com.ar/go/chasquid/internal/testlib"
	"blitiri.com.ar/go/chasquid/internal/trace"
)

// This domain will cause idna.ToASCII to fail.
var invalidDomain = "test " + strings.Repeat("x", 65536) + "\uff00"

// Override the netLookupMX function, to return controlled results for
// testing.
var testMX = map[string][]*net.MX{}
var testMXErr = map[string]error{}

func init() {
	netLookupMX = func(name string) ([]*net.MX, error) {
		return testMX[name], testMXErr[name]
	}
}

func newSMTP(t *testing.T) (*SMTP, string) {
	dir := testlib.MustTempDir(t)
	dinfo, err := domaininfo.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	return &SMTP{"hello", dinfo, nil}, dir
}

func TestSMTP(t *testing.T) {
	// Shorten the total timeout, so the test fails quickly if the protocol
	// gets stuck.
	smtpTotalTimeout = 5 * time.Second

	responses := map[string]string{
		"_welcome":          "220 welcome\n",
		"EHLO hello":        "250 ehlo ok\n",
		"MAIL FROM:<me@me>": "250 mail ok\n",
		"RCPT TO:<to@to>":   "250 rcpt ok\n",
		"DATA":              "354 send data\n",
		"_DATA":             "250 data ok\n",
		"QUIT":              "250 quit ok\n",
	}
	srv := newFakeServer(t, responses)
	host, port, _ := net.SplitHostPort(srv.addr)

	// Put a non-existing host first, so we check that if the first host
	// doesn't work, we try with the rest.
	// The host we use is invalid, to avoid having to do an actual network
	// lookup whick makes the test more hermetic. This is a hack, ideally we
	// would be able to override the default resolver, but Go does not
	// implement that yet.
	testMX["to"] = []*net.MX{
		{Host: ":::", Pref: 10},
		{Host: host, Pref: 20},
	}
	*smtpPort = port

	s, tmpDir := newSMTP(t)
	defer testlib.RemoveIfOk(t, tmpDir)
	err, _ := s.Deliver("me@me", "to@to", []byte("data"))
	if err != nil {
		t.Errorf("deliver failed: %v", err)
	}

	srv.wg.Wait()
}

func TestSMTPErrors(t *testing.T) {
	// Shorten the total timeout, so the test fails quickly if the protocol
	// gets stuck.
	smtpTotalTimeout = 1 * time.Second

	cases := []struct {
		responses map[string]string
		errPrefix string
	}{
		// First test: hang response, should fail due to timeout.
		{
			makeResp("_welcome", "220 no newline"),
			"",
		},

		// MAIL FROM not allowed.
		{
			makeResp(
				"_welcome", "220 mail from not allowed\n",
				"EHLO hello", "250 ehlo ok\n",
				"MAIL FROM:<me@me>", "501 mail error\n",
			),
			"MAIL+RCPT 501 mail error",
		},

		// RCPT TO not allowed.
		{
			makeResp(
				"_welcome", "220 rcpt to not allowed\n",
				"EHLO hello", "250 ehlo ok\n",
				"MAIL FROM:<me@me>", "250 mail ok\n",
				"RCPT TO:<to@to>", "501 rcpt error\n",
			),
			"MAIL+RCPT 501 rcpt error",
		},

		// DATA error.
		{
			makeResp(
				"_welcome", "220 data error\n",
				"EHLO hello", "250 ehlo ok\n",
				"MAIL FROM:<me@me>", "250 mail ok\n",
				"RCPT TO:<to@to>", "250 rcpt ok\n",
				"DATA", "554 data error\n",
			),
			"DATA 554 data error",
		},

		// DATA response error.
		{
			makeResp(
				"_welcome", "220 data error\n",
				"EHLO hello", "250 ehlo ok\n",
				"MAIL FROM:<me@me>", "250 mail ok\n",
				"RCPT TO:<to@to>", "250 rcpt ok\n",
				"DATA", "354 send data\n",
				"_DATA", "551 data response error\n",
			),
			"DATA closing 551 data response error",
		},
	}

	for _, c := range cases {
		srv := newFakeServer(t, c.responses)
		host, port, _ := net.SplitHostPort(srv.addr)

		testMX["to"] = []*net.MX{{Host: host, Pref: 10}}
		*smtpPort = port

		s, tmpDir := newSMTP(t)
		defer testlib.RemoveIfOk(t, tmpDir)
		err, _ := s.Deliver("me@me", "to@to", []byte("data"))

		if err == nil {
			t.Errorf("deliver not failed in case %q: %v",
				c.responses["_welcome"], err)
			continue
		}
		t.Logf("failed as expected: %v", err)

		if !strings.HasPrefix(err.Error(), c.errPrefix) {
			t.Errorf("expected error prefix %q, got %q",
				c.errPrefix, err)
		}

		srv.wg.Wait()
	}
}

func TestNoMXServer(t *testing.T) {
	testMX["to"] = []*net.MX{}

	s, tmpDir := newSMTP(t)
	defer testlib.RemoveIfOk(t, tmpDir)
	err, permanent := s.Deliver("me@me", "to@to", []byte("data"))
	if err == nil {
		t.Errorf("delivery worked, expected failure")
	}
	if !permanent {
		t.Errorf("expected permanent failure, got transient (%v)", err)
	}
	t.Logf("got permanent failure, as expected: %v", err)
}

func TestTooManyMX(t *testing.T) {
	tr := trace.New("test", "test")
	testMX["domain"] = []*net.MX{
		{Host: "h1", Pref: 10}, {Host: "h2", Pref: 20},
		{Host: "h3", Pref: 30}, {Host: "h4", Pref: 40},
		{Host: "h5", Pref: 50}, {Host: "h5", Pref: 60},
	}
	mxs, err, perm := lookupMXs(tr, "domain")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if perm != true {
		t.Fatalf("expected perm == true")
	}
	if len(mxs) != 5 {
		t.Errorf("expected len(mxs) == 5, got: %v", mxs)
	}
}

func TestFallbackToA(t *testing.T) {
	tr := trace.New("test", "test")
	testMX["domain"] = nil
	testMXErr["domain"] = &net.DNSError{
		Err:         "no such host (test)",
		IsTemporary: false,
	}

	mxs, err, perm := lookupMXs(tr, "domain")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if perm != true {
		t.Fatalf("expected perm == true")
	}
	if !(len(mxs) == 1 && mxs[0] == "domain") {
		t.Errorf("expected mxs == [domain], got: %v", mxs)
	}
}

func TestTemporaryDNSerror(t *testing.T) {
	tr := trace.New("test", "test")
	testMX["domain"] = nil
	testMXErr["domain"] = &net.DNSError{
		Err:         "temp error (test)",
		IsTemporary: true,
	}

	mxs, err, perm := lookupMXs(tr, "domain")
	if !(mxs == nil && err == testMXErr["domain"]) {
		t.Errorf("expected mxs == nil, err == test error, got: %v, %v", mxs, err)
	}
	if perm != false {
		t.Errorf("expected perm == false")
	}
}

func TestMXLookupError(t *testing.T) {
	tr := trace.New("test", "test")
	testMX["domain"] = nil
	testMXErr["domain"] = fmt.Errorf("test error")

	mxs, err, perm := lookupMXs(tr, "domain")
	if !(mxs == nil && err == testMXErr["domain"]) {
		t.Errorf("expected mxs == nil, err == test error, got: %v, %v", mxs, err)
	}
	if perm != true {
		t.Fatalf("expected perm == true")
	}
}

func TestLookupInvalidDomain(t *testing.T) {
	tr := trace.New("test", "test")

	mxs, err, perm := lookupMXs(tr, invalidDomain)
	if !(mxs == nil && err != nil) {
		t.Errorf("expected err != nil, got: %v, %v", mxs, err)
	}
	if perm != true {
		t.Fatalf("expected perm == true")
	}
}

// TODO: Test STARTTLS negotiation.
