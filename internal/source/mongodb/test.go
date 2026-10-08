package mongodb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"lakesync/internal/core"
)

// EgressIPPlaceholder is substituted by the API with the server's public IP.
const EgressIPPlaceholder = "{egress_ip}"

// Hints shown next to common failures (brief section 10.1).
const (
	hintAllowlist     = "Atlas may be blocking this server's IP. Add " + EgressIPPlaceholder + " in Atlas → Network Access."
	hintAuth          = "Check the username and password, and that the user's auth database is right (usually `admin`)."
	hintReplSet       = "Change streams need a replica set. Atlas clusters always are; for self-hosted MongoDB, start with `--replSet`."
	hintWatchRole     = "Give this user the `read` role on the database (it includes `changeStream`)."
	hintDNS           = "The host name does not resolve. Check the cluster address in the connection string and this server's DNS."
	hintTLS           = "The TLS handshake failed. Atlas requires TLS; for self-hosted servers make sure `tls=true` matches the server."
	hintNoCollections = "No user collections were found; create one or point the connection at the right database."
)

// Test runs the ordered checklist of brief section 10.1.
func (s *Source) Test(ctx context.Context) core.TestReport {
	rep := core.TestReport{Info: map[string]string{}}
	step := func(name string, fn func() (status core.TestStatus, msg, hint string)) bool {
		start := time.Now()
		status, msg, hint := fn()
		rep.Add(core.TestStep{Name: name, Status: status, Duration: time.Since(start), Message: msg, Hint: hint})
		return status != core.TestFail
	}

	// 1. parse
	var parsed *ParsedURI
	if !step("Parse connection string", func() (core.TestStatus, string, string) {
		p, err := ParseURI(s.cfg.URI)
		if err != nil {
			return core.TestFail, err.Error(), "Paste the connection string from Atlas → Connect → Drivers, or check it for typos."
		}
		parsed = p
		kind := "mongodb://"
		if p.SRV {
			kind = "mongodb+srv://"
		}
		return core.TestOK, fmt.Sprintf("%s with %d host(s)%s", kind, len(p.Hosts), map[bool]string{true: ", TLS", false: ""}[p.TLS()]), ""
	}) {
		return rep
	}

	// 2. DNS / SRV
	var targets []string // host:port to dial
	if !step("DNS / SRV lookup", func() (core.TestStatus, string, string) {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if parsed.SRV {
			_, addrs, err := net.DefaultResolver.LookupSRV(rctx, "mongodb", "tcp", parsed.Hosts[0])
			if err != nil || len(addrs) == 0 {
				return core.TestFail, fmt.Sprintf("no SRV record _mongodb._tcp.%s: %v", parsed.Hosts[0], errString(err)), hintDNS
			}
			for _, a := range addrs {
				targets = append(targets, net.JoinHostPort(strings.TrimSuffix(a.Target, "."), strconv.Itoa(int(a.Port))))
			}
			if _, err := net.DefaultResolver.LookupTXT(rctx, parsed.Hosts[0]); err != nil {
				return core.TestOK, fmt.Sprintf("%d SRV targets (no TXT options)", len(addrs)), ""
			}
			return core.TestOK, fmt.Sprintf("%d SRV targets", len(addrs)), ""
		}
		for _, h := range parsed.Hosts {
			host, port := splitHostPort(h, "27017")
			if ip := net.ParseIP(host); ip == nil {
				if _, err := net.DefaultResolver.LookupHost(rctx, host); err != nil {
					return core.TestFail, fmt.Sprintf("cannot resolve %s: %v", host, err), hintDNS
				}
			}
			targets = append(targets, net.JoinHostPort(host, port))
		}
		return core.TestOK, fmt.Sprintf("%d host(s) resolved", len(targets)), ""
	}) {
		return rep
	}

	// 3. TCP + TLS
	if !step("TCP and TLS", func() (core.TestStatus, string, string) {
		dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		target := targets[0]
		d := net.Dialer{}
		conn, err := d.DialContext(dctx, "tcp", target)
		if err != nil {
			hint := "Check the host and port, and that a firewall allows this server to reach it."
			if parsed.IsAtlas() {
				hint = hintAllowlist
			}
			return core.TestFail, fmt.Sprintf("cannot connect to %s: %v", target, err), hint
		}
		defer conn.Close()
		if !parsed.TLS() {
			return core.TestOK, "TCP connection to " + target + " (no TLS)", ""
		}
		host, _ := splitHostPort(target, "")
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(dctx); err != nil {
			return core.TestFail, fmt.Sprintf("TLS handshake with %s failed: %v", target, err), hintTLS
		}
		return core.TestOK, fmt.Sprintf("TLS %s to %s", tlsVersion(tc.ConnectionState().Version), target), ""
	}) {
		return rep
	}

	// 4. authenticate
	if !step("Authenticate", func() (core.TestStatus, string, string) {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := s.pingPrimary(pctx); err != nil {
			err = s.redact(err)
			switch {
			case isAuthError(err):
				return core.TestFail, "authentication failed", hintAuth
			case parsed.IsAtlas() && (mongo.IsTimeout(err) || mongo.IsNetworkError(err) || isServerSelection(err)):
				return core.TestFail, "timed out reaching the cluster: " + err.Error(), hintAllowlist
			case isServerSelection(err):
				return core.TestFail, "no reachable server: " + err.Error(), "The hosts answered on TCP but the driver could not select a server; check `replicaSet`/`directConnection` options and that the members' advertised host names resolve from here."
			}
			return core.TestFail, err.Error(), "Check the connection string and credentials."
		}
		if s.cfg.Username == "" {
			return core.TestOK, "connected without credentials", ""
		}
		return core.TestOK, "authenticated as " + s.cfg.Username, ""
	}) {
		return rep
	}

	// 5. hello -> replica set
	if !step("Replica set", func() (core.TestStatus, string, string) {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		raw, err := s.client.Database("admin").RunCommand(hctx, bson.D{{Key: "hello", Value: 1}}).Raw()
		if err != nil {
			return core.TestFail, "hello failed: " + s.redact(err).Error(), ""
		}
		if v, err := s.client.Database("admin").RunCommand(hctx, bson.D{{Key: "buildInfo", Value: 1}}).Raw(); err == nil {
			if ver, ok := v.Lookup("version").StringValueOK(); ok {
				rep.Info["server_version"] = ver
			}
		}
		setName, _ := raw.Lookup("setName").StringValueOK()
		msg, _ := raw.Lookup("msg").StringValueOK()
		primary, _ := raw.Lookup("isWritablePrimary").BooleanOK()
		if hosts, err := raw.LookupErr("hosts"); err == nil {
			if vals, err := hosts.Array().Values(); err == nil {
				rep.Info["hosts"] = strconv.Itoa(len(vals))
			}
		}
		switch {
		case msg == "isdbgrid":
			rep.Info["topology"] = "sharded cluster"
			return core.TestOK, "sharded cluster (mongos)", ""
		case setName != "":
			rep.Info["topology"] = "replica set " + setName
			if !primary {
				return core.TestWarn, fmt.Sprintf("replica set %s, but this member is not the primary", setName), "Change streams work, but writes and some reads go to the primary; the driver will route them."
			}
			return core.TestOK, fmt.Sprintf("replica set %s, primary", setName), ""
		}
		rep.Info["topology"] = "standalone"
		return core.TestFail, "the server is a standalone instance", hintReplSet
	}) {
		return rep
	}

	// 6. list databases
	var entities []core.Entity
	if !step("List databases", func() (core.TestStatus, string, string) {
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		ents, err := s.ListEntities(lctx)
		if err != nil {
			hint := ""
			if isUnauthorized(err) {
				hint = "Give this user `readAnyDatabase` (or `read` on the databases to sync) so lakesync can list them."
			}
			return core.TestFail, err.Error(), hint
		}
		entities = ents
		dbs := map[string]bool{}
		for _, e := range ents {
			dbs[e.Namespace] = true
		}
		if len(ents) == 0 {
			return core.TestWarn, "no user collections found", hintNoCollections
		}
		return core.TestOK, fmt.Sprintf("%d database(s), %d collection(s)", len(dbs), len(ents)), ""
	}) {
		return rep
	}

	// 7. change stream privilege
	if len(entities) == 0 {
		rep.Add(core.TestStep{Name: "Open a change stream", Status: core.TestWarn, Message: "skipped: no collection to watch", Hint: hintNoCollections})
		rep.Add(core.TestStep{Name: "Read one document", Status: core.TestWarn, Message: "skipped: no collection to read"})
		return rep
	}
	first := entities[0]
	step("Open a change stream", func() (core.TestStatus, string, string) {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cs, err := s.coll(first).Watch(wctx, mongo.Pipeline{}, options.ChangeStream().SetMaxAwaitTime(200*time.Millisecond))
		if err != nil {
			if isUnauthorized(err) {
				return core.TestFail, fmt.Sprintf("not allowed to watch %s", first), hintWatchRole
			}
			return core.TestFail, fmt.Sprintf("cannot open a change stream on %s: %v", first, s.redact(err)), ""
		}
		defer cs.Close(wctx)
		cs.TryNext(wctx)
		if err := cs.Err(); err != nil && wctx.Err() == nil {
			if isUnauthorized(err) {
				return core.TestFail, fmt.Sprintf("not allowed to watch %s", first), hintWatchRole
			}
			return core.TestFail, fmt.Sprintf("change stream on %s failed: %v", first, s.redact(err)), ""
		}
		if enabled, known := s.PrePostImagesEnabled(first); known {
			rep.Info["pre_post_images"] = strconv.FormatBool(enabled)
		} else {
			s.recordPrePostImages(wctx, first)
			enabled, _ := s.PrePostImagesEnabled(first)
			rep.Info["pre_post_images"] = strconv.FormatBool(enabled)
		}
		return core.TestOK, "change stream opened on " + first.String(), ""
	})

	// 8. read one document
	step("Read one document", func() (core.TestStatus, string, string) {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res := s.coll(first).FindOne(rctx, bson.D{})
		raw, err := res.Raw()
		if errors.Is(err, mongo.ErrNoDocuments) {
			return core.TestWarn, first.String() + " is empty", "An empty collection syncs fine; new documents arrive through the change stream."
		}
		if err != nil {
			if isUnauthorized(err) {
				return core.TestFail, "not allowed to read " + first.String(), hintWatchRole
			}
			return core.TestFail, "read failed: " + s.redact(err).Error(), ""
		}
		if _, err := toDocument(raw); err != nil {
			return core.TestFail, "document could not be converted: " + err.Error(), ""
		}
		return core.TestOK, "read a document from " + first.String(), ""
	})
	return rep
}

func errString(err error) string {
	if err == nil {
		return "no records"
	}
	return err.Error()
}

func splitHostPort(h, defPort string) (string, string) {
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		return h, defPort
	}
	return host, port
}

func tlsVersion(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	}
	return fmt.Sprintf("0x%x", v)
}

// isAuthError reports whether err is an authentication failure.
func isAuthError(err error) bool {
	var se mongo.ServerError
	if errors.As(err, &se) && (se.HasErrorCode(18) || se.HasErrorCode(11)) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "authentication failed") || strings.Contains(msg, "auth error") || strings.Contains(msg, "unable to authenticate")
}

// isUnauthorized reports whether err is an authorization (role) failure.
func isUnauthorized(err error) bool {
	var se mongo.ServerError
	if errors.As(err, &se) && se.HasErrorCode(13) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not authorized") || strings.Contains(msg, "unauthorized")
}

// isServerSelection reports whether err is a server selection timeout.
func isServerSelection(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "server selection")
}
