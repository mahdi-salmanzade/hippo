package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mahdi-salmanzade/hippo/mcp"
)

// parseMCPForm reads MCP form fields into an MCPConfig. The form
// convention is: one or more server rows indexed mcp_name_0,
// mcp_name_1, …, plus a single mcp_add flag to append a new empty
// row, and mcp_delete to mark a row for removal.
func parseMCPForm(r *http.Request) MCPConfig {
	servers := []MCPServerConfig{}
	// Scan indices by looking at form keys matching mcp_name_N.
	indexes := map[int]bool{}
	for key := range r.Form {
		if !strings.HasPrefix(key, "mcp_name_") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(key, "mcp_name_"))
		if err != nil {
			continue
		}
		indexes[n] = true
	}

	deleteIdx := -1
	if v := r.FormValue("mcp_delete"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			deleteIdx = n
		}
	}

	for i := 0; i < len(indexes)+1; i++ {
		if !indexes[i] {
			continue
		}
		if i == deleteIdx {
			continue
		}
		name := strings.TrimSpace(r.FormValue(fmt.Sprintf("mcp_name_%d", i)))
		if name == "" {
			continue
		}
		transport := strings.TrimSpace(r.FormValue(fmt.Sprintf("mcp_transport_%d", i)))
		cmdOrURL := strings.TrimSpace(r.FormValue(fmt.Sprintf("mcp_command_%d", i)))
		prefix := strings.TrimSpace(r.FormValue(fmt.Sprintf("mcp_prefix_%d", i)))
		enabled := r.FormValue(fmt.Sprintf("mcp_enabled_%d", i)) == "on"

		s := MCPServerConfig{
			Name:      name,
			Transport: transport,
			Prefix:    prefix,
			Enabled:   enabled,
		}
		switch transport {
		case "stdio":
			s.Command = splitCommand(cmdOrURL)
		case "http":
			s.URL = cmdOrURL
		}
		servers = append(servers, s)
	}

	// Handle the "add server" submit: append a fresh disabled entry.
	if r.FormValue("mcp_add") != "" {
		servers = append(servers, MCPServerConfig{
			Name:      fmt.Sprintf("server-%d", len(servers)+1),
			Transport: "stdio",
			Enabled:   false,
		})
	}

	return MCPConfig{Servers: servers}
}

// splitCommand breaks a user-entered command on whitespace, honoring
// simple "quoted args". A full shell parse is overkill - this mirrors
// what strings.Fields does plus a single level of double-quote
// tolerance so users can type `echo "hello world"`.
func splitCommand(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
		case !inQuote && (ch == ' ' || ch == '\t'):
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// handleMCPTest performs a single live-connect round-trip against the
// supplied server config and returns the count of tools it exposes.
// Used by the config page's "Test" button before saving. Timeout
// matches bundle construction (10s).
//
// The form uses either flat ("name", "transport", "command", "prefix")
// or row-indexed ("mcp_name_N", …) field names - the config page's
// htmx include copies the row's existing inputs rather than requiring
// a separate hidden mirror.
func (s *Server) handleMCPTest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeTestResult(w, false, "bad form: "+err.Error(), 0, "")
		return
	}
	name := firstNonEmpty(r, "name", "mcp_name_")
	transport := firstNonEmpty(r, "transport", "mcp_transport_")
	cmdOrURL := firstNonEmpty(r, "command", "mcp_command_")
	prefix := firstNonEmpty(r, "prefix", "mcp_prefix_")
	name = strings.TrimSpace(name)
	transport = strings.TrimSpace(transport)
	cmdOrURL = strings.TrimSpace(cmdOrURL)
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = name
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		client *mcp.Client
		err    error
	)
	switch transport {
	case "stdio":
		argv := splitCommand(cmdOrURL)
		if len(argv) == 0 {
			writeTestResult(w, false, "command is empty", 0, "")
			return
		}
		client, err = mcp.Connect(ctx, argv,
			mcp.WithPrefix(prefix),
			mcp.WithLogger(s.logger),
			mcp.WithReconnect(false, 0, 0),
		)
	case "http":
		client, err = mcp.ConnectHTTP(ctx, cmdOrURL,
			mcp.WithPrefix(prefix),
			mcp.WithLogger(s.logger),
			mcp.WithReconnect(false, 0, 0),
		)
	default:
		writeTestResult(w, false, "unknown transport "+transport, 0, "")
		return
	}
	if err != nil {
		writeTestResult(w, false, err.Error(), 0, "")
		return
	}
	defer client.Close()

	writeTestResult(w, true, "", len(client.Tools()), client.Name())
}

// firstNonEmpty returns the first non-empty value matching flat or
// any row-indexed form field. Row-indexed names take precedence
// because the config page submits one row at a time through htmx.
func firstNonEmpty(r *http.Request, flat, prefix string) string {
	for key, vals := range r.Form {
		if strings.HasPrefix(key, prefix) && len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return r.FormValue(flat)
}

// writeTestResult renders a short HTML fragment reporting the
// test-connect outcome. htmx swaps this into the status span next to
// the Test button.
func writeTestResult(w http.ResponseWriter, ok bool, errMsg string, toolCount int, serverName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if ok {
		_, _ = fmt.Fprintf(w, `<span class="ok">✓ %d tool(s) from %q</span>`, toolCount, serverName)
		return
	}
	_, _ = fmt.Fprintf(w, `<span class="err">✗ %s</span>`, errMsg)
}
