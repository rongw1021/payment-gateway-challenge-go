package scenarios

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const divider = "=============================================================================="

var httpVerbose = flag.Bool("http-verbose", false, "Log full unredacted development-test HTTP requests and responses")

func describeRequest(input request) string {
	if input.description != "" {
		return input.description
	}
	switch {
	case input.method == http.MethodPost && input.path == "/api/CardTokens":
		return "Exchange card details for a single-use token"
	case input.method == http.MethodPost && input.path == "/api/PaymentIntents":
		return "Create or replay a payment intent"
	case input.method == http.MethodPost && strings.HasSuffix(input.path, "/confirm"):
		return "Confirm or replay a payment attempt"
	case input.method == http.MethodGet && strings.HasPrefix(input.path, "/api/PaymentIntents/"):
		return "Retrieve a payment intent and its transaction history"
	default:
		return input.method + " " + input.path
	}
}

func formatExchange(testName string, input request, req *http.Request, requestBody []byte, reply response, responseBody []byte, err error) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s\nTest case: %s\nDescription: %s\n", divider, testName, describeRequest(input))
	if req != nil {
		fmt.Fprintf(&out, "Request: %s %s\nRequest headers (explicit):\n", req.Method, req.URL)
		writeHeaders(&out, req.Header)
	} else {
		fmt.Fprintf(&out, "Request: %s %s (not sent)\nRequest headers: (not constructed)\n", input.method, input.path)
	}
	writeBody(&out, "Request", requestBody)
	if reply.status != 0 {
		fmt.Fprintf(&out, "Response status: %d %s\nResponse headers:\n", reply.status, http.StatusText(reply.status))
		writeHeaders(&out, reply.headers)
	} else {
		out.WriteString("Response status: (not received)\nResponse headers: (none)\n")
	}
	writeBody(&out, "Response", responseBody)
	if err != nil {
		fmt.Fprintf(&out, "Error: %v\n", err)
	}
	fmt.Fprintln(&out, divider)
	return out.String()
}

func writeHeaders(out *strings.Builder, headers http.Header) {
	if len(headers) == 0 {
		out.WriteString("  (none)\n")
		return
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range headers[name] {
			fmt.Fprintf(out, "  %s: %s\n", name, value)
		}
	}
}

func writeBody(out *strings.Builder, label string, body []byte) {
	fmt.Fprintf(out, "%s body (%d bytes):\n", label, len(body))
	if len(body) == 0 {
		out.WriteString("  (none)\n")
		return
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, bytes.TrimSpace(body), "  ", "  "); err != nil {
		out.WriteString("  (non-JSON body)\n")
		out.Write(body)
	} else {
		out.WriteString("  ")
		out.Write(formatted.Bytes())
	}
	out.WriteByte('\n')
}
