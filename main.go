package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func main() {
	cmd := &cobra.Command{
		Use:   "http-assert <URL>",
		Short: "Perform HTTP request and assert received HTTP response",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			httpClient := getHttpClient(parseHostMappings(cmd))

			m, _ := cmd.Flags().GetString("request")
			b := io.Reader(http.NoBody)
			if d, _ := cmd.Flags().GetString("data"); d != "" {
				b = strings.NewReader(d)
			}
			req, err := http.NewRequestWithContext(cmd.Context(), m, args[0], b)
			if err != nil {
				die(91, "Cannot create %s request: %s", m, err)
			}

			if err := assertRequest(httpClient, req, parseAssertionFlags(cmd)...); err != nil {
				die(93, "Cannot create %s request: %s", m, err)
			}
		},
	}
	cmd.PersistentFlags().StringArray("maphost", nil,
		// [:dstport] is an addition to curl's --resolve
		"Provide a custom address for a specific host and port pair; e.g. <host:port:addr[:dstport]...>")
	cmd.Flags().StringP("request", "X", "GET",
		"Specifies a custom request method to use when communicating with the HTTP server")
	cmd.Flags().StringP("data", "d", "",
		"Sends the specified data in a POST request to the HTTP server")
	registerAssertionFlags(cmd)

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		die(103, "%s", err)
	}
}

func die(rc int, format string, args ...interface{}) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	fmt.Fprintf(os.Stderr, "Error: "+format, args...)
	os.Exit(rc)
}

func parseHostMappings(cmd *cobra.Command) []hostMapping {
	var res []hostMapping

	vals, _ := cmd.Flags().GetStringArray("maphost")
	for _, r := range vals {
		// format: src-hostname:src-port:dst-hostname:dst-port
		i := strings.Index(r, ":")
		if i < 0 {
			die(91, "Invalid value for --maphost flag: %q", r)
		}

		j := strings.Index(r[i+1:], ":")
		if j < 0 {
			die(91, "Invalid value for --maphost flag: %q", r)
		}

		r := hostMapping{Src: r[:i+1+j], Dst: r[i+1+j+1:]}
		res = append(res, r)
	}

	return res
}

func registerAssertionFlags(cmd *cobra.Command) {
	cmd.Flags().Int("assert-status", 0, "Assert response status equals the provided value")
	cmd.Flags().StringArray("assert-header", nil, "Assert header equals the provided regexp")
	cmd.Flags().StringP("assert-body", "B", "", "Assert body equals the provided value")

	// Common shorthands
	cmd.Flags().Bool("assert-ok", false, "Assert response is successful (2xx)")
	cmd.Flags().String("assert-redirect", "", "Assert response redirects to the provided URL")
}

func parseAssertionFlags(cmd *cobra.Command) []Assertion {
	var res []Assertion

	if cmd.Flags().Changed("assert-ok") {
		if v, _ := cmd.Flags().GetBool("assert-ok"); v {
			res = append(res, AssertStatusOK())
		} else {
			res = append(res, AssertStatusNOK())
		}
	}

	if cmd.Flags().Changed("assert-redirect") {
		v, _ := cmd.Flags().GetString("assert-redirect")
		if strings.HasPrefix(v, "=") {
			res = append(res, AssertRedirectEqual(v[1:]))
		} else {
			res = append(res, AssertRedirectMatch(v))
		}
	}

	if cmd.Flags().Changed("assert-status") {
		s, _ := cmd.Flags().GetInt("assert-status")
		res = append(res, AssertStatusEqual(s))
	}

	hs, _ := cmd.Flags().GetStringArray("assert-header")
	for _, h := range hs {
		parts := strings.SplitN(h, ":", 2)
		name := strings.TrimSpace(parts[0])
		var value string
		if len(parts) > 1 {
			value = strings.TrimSpace(parts[1])
		}
		var exactMatch bool
		if strings.HasPrefix(name, "=") {
			name = name[1:]
			exactMatch = true
		}

		if exactMatch {
			if value == "" {
				res = append(res, AssertHeaderPresent(name))
			} else {
				res = append(res, AssertHeaderEqual(name, value))
			}
		} else {
			if value == "" {
				res = append(res, AssertHeaderPresent(name))
			} else {
				res = append(res, AssertHeaderMatch(name, value))
			}
		}
	}

	if cmd.Flags().Changed("assert-body") {
		v, _ := cmd.Flags().GetString("assert-body")
		if strings.HasPrefix(v, "=") {
			res = append(res, AssertBodyEqual(v[1:]))
		} else {
			res = append(res, AssertBodyMatch(v))
		}
	}

	return res
}

func assertRequest(httpClient *http.Client, req *http.Request, assertions ...Assertion) error {
	if len(assertions) == 0 {
		return fmt.Errorf("no assertions defined")
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer res.Body.Close()

	httpRes := &httpResponse{Response: res}
	httpRes.BodyBytes, _ = io.ReadAll(res.Body)

	var assertErrors []error
	for i := range assertions {
		if err := assertions[i](httpRes); err != nil {
			assertErrors = append(assertErrors, err)
		}
	}
	if len(assertErrors) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d assertions failed:\n", len(assertErrors))
		for i := range assertErrors {
			fmt.Fprintf(&b, "- %s\n", assertErrors[i])
		}
		b.WriteString("\n\n")
		httpRes.writeTo(&b, true)
		b.WriteString("\n")
		return errors.New(b.String())
	}

	return nil
}

type httpResponse struct {
	*http.Response
	BodyBytes []byte
}

func (r httpResponse) writeTo(w io.Writer, withBody bool) {
	// Ensure to close previous body
	b := r.Response.Body
	defer b.Close()
	if withBody {
		r.Response.Body = io.NopCloser(bytes.NewReader(r.BodyBytes))
	} else {
		r.Response.Body = io.NopCloser(strings.NewReader("<<Payload is omitted>>"))
	}
	r.Response.Write(w)
}

func getHttpClient(hostMappings []hostMapping) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 20 * time.Second,
	}

	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Disallow redirects
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          10,
			IdleConnTimeout:       20 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			Proxy:                 http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				for _, r := range hostMappings {
					if r.Matches(addr) {
						addr = r.DstHost()
						break
					}
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

type hostMapping struct {
	// Src is the source host in the form of `hostname:port`.
	Src string
	// Dst is the destination host in the form of either `hostname:port` or just
	// `hostname`. If just the hostname is specified without a port then the
	// source port will be used.
	Dst string
}

func (r hostMapping) Matches(addr string) bool {
	return r.Src == addr
}

func (r hostMapping) DstHost() string {
	// Dst already has a port
	if idx := strings.Index(r.Dst, ":"); idx >= 0 {
		return r.Dst
	}

	// Use the source port
	var port string
	if idx := strings.Index(r.Src, ":"); idx >= 0 {
		port = r.Src[idx:]
	}
	return r.Dst + port
}
