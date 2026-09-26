package tui

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// A connection card is composed from the minted token, the actual front-door
// endpoint, and the owner pinned to the same Bound that minted it. In
// particular, neither a target database password nor a later session user
// belongs here. The whole card is built ONCE and copied from that same string.
func (h *Host) showConnectionCard(out PATSecret, conn ConnInfo, ep FrontDoorEndpoint, owner UserInfo) {
	user := owner.Name
	if user == "" {
		user = "your-autodb-user"
	}
	h.cardText = buildCardText(out.Secret, conn, ep, user, owner.Role, out.ExpiresAt)
	h.set("App.cardText", h.cardText)
	h.set("App.cardTitle", "token "+out.Name+" (shown once)")
	h.open("card")
}

func (h *Host) copyCard() error {
	if h.cardText == "" {
		return nil
	}
	h.editor.SetRegister(h.cardText, false)
	if h.p.App().CopyToClipboard(h.cardText) {
		h.setStatus("whole card copied to clipboard and editor register")
	} else {
		h.setStatus("clipboard unavailable — whole card copied to editor register")
	}
	return nil
}

// Once closed, a show-once secret is no longer readable from the host source.
func (h *Host) cardClosed() error {
	h.cardText = ""
	h.set("App.cardText", "")
	h.set("App.cardTitle", "")
	return nil
}

func cardDatabase(c ConnInfo) string {
	if c.TargetDB != "" {
		return c.TargetDB
	}
	return c.Name
}

func cardCap(n int) string {
	if n <= 0 {
		return "not reported"
	}
	return fmt.Sprint(n)
}

func cardSSLMode(ep FrontDoorEndpoint) string {
	if ep.Cleartext {
		return "disable"
	}
	return "verify-full"
}

// buildCardText includes exactly stable ceilings, never live availability or
// a per-source concurrency claim. A failure-rate throttle is NOT a pool cap.
func buildCardText(secret string, conn ConnInfo, ep FrontDoorEndpoint, user, role, expires string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	if !ep.Configured() {
		line("!! THIS TOKEN CANNOT BE USED YET.")
		if !ep.Enabled {
			line("!! The front door is not enabled on this install.")
		} else {
			line("!! The front door is enabled but NO LISTENER IS RUNNING.")
		}
		line("")
	}
	if ep.Configured() && ep.Cleartext {
		line("!! THIS FRONT DOOR IS SERVING WITHOUT TLS.")
		line("!! The token below crosses the network in CLEARTEXT. Treat it as exposed.")
		line("")
	}
	host, port, err := net.SplitHostPort(ep.Addr)
	if err != nil {
		host, port = ep.Addr, ""
	}
	if host == "0.0.0.0" || host == "::" {
		host = ""
	}
	if len(ep.HostNames) > 0 {
		host = ep.HostNames[0]
	}
	if host == "" {
		host = "(unknown)"
	}
	if port == "" {
		port = "(unknown)"
	}
	database := cardDatabase(conn)
	sslmode := cardSSLMode(ep)
	line("connection   %s", conn.Name)
	line("database     %s        <- use this in the client Database field", database)
	line("host         %s", host)
	line("port         %s", port)
	if role != "" {
		line("user         %s        (role %s)", user, role)
	} else {
		line("user         %s", user)
	}
	line("sslmode      %s", sslmode)
	if ep.RootCAFile != "" && !ep.Cleartext {
		line("sslrootcert  %s", ep.RootCAFile)
	}
	if expires != "" {
		line("expires      %s", expires)
	}
	line("")
	line("token        %s", secret)
	line("The token is shown ONCE and cannot be recovered. Copy it before closing.")
	q := url.Values{"sslmode": {sslmode}}
	if ep.RootCAFile != "" && !ep.Cleartext {
		q.Set("sslrootcert", ep.RootCAFile)
	}
	endpoint := net.JoinHostPort(host, port)
	username := url.UserPassword(user, secret).String()
	path := url.PathEscape(database) // one database name, even if it contains /
	line("DSN")
	line("  postgres://%s@%s/%s?%s", username, endpoint, path, q.Encode())
	line("JDBC")
	jdbcSSL := "true"
	if ep.Cleartext {
		jdbcSSL = "false"
	}
	jdbc := url.Values{"user": {user}, "password": {secret}, "ssl": {jdbcSSL}, "sslmode": {sslmode}}
	if ep.RootCAFile != "" && !ep.Cleartext {
		jdbc.Set("sslrootcert", ep.RootCAFile)
	}
	line("  jdbc:postgresql://%s/%s?%s", endpoint, path, jdbc.Encode())
	line("")
	if ep.Configured() {
		writeCardBudget(line, conn, ep)
	}
	return b.String()
}

func writeCardBudget(line func(string, ...any), conn ConnInfo, ep FrontDoorEndpoint) {
	line("LIMITS THAT APPLY TO THIS TOKEN")
	line("  You do not size anything. autodb holds you to these whether or not your")
	line("  client is configured for them. These ceilings are shared, not yours alone.")
	line("")
	if conn.PoolMaxConns > 0 {
		line("  %-22s %-9s %s", "this connection", cardCap(conn.PoolMaxConns), "pooled connections to "+cardDatabase(conn))
	}
	line("  %-22s %-9s %s", "sessions per user", cardCap(ep.MaxSessionsPerUser), "you, across databases")
	line("  %-22s %-9s %s", "sessions, instance", cardCap(ep.MaxSessionsGlobal), "everyone using this autodb")
	line("  %-22s %-9s %s", "backend connections", cardCap(ep.MaxTargetConns), "everyone, across target databases")
	line("")
	line("  For live availability or refusal reasons, open System -> Pressure.")
}
