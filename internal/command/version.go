package command

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newVersionCommand(env *Env) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := env.Writer()
			if err != nil {
				return err
			}
			info := map[string]any{
				"version":  Version,
				"go":       runtime.Version(),
				"platform": runtime.GOOS + "/" + runtime.GOARCH,
			}
			if check {
				latest, err := latestVersion(cmd, env)
				if err != nil {
					info["update_check"] = "could not reach the platform"
				} else {
					info["latest"] = latest
					info["up_to_date"] = latest == "" || latest == Version
				}
			}
			return w.Object(info)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "ask the platform whether a newer version exists")
	return cmd
}

// latestVersion reports what the platform says is current. It never downloads
// or executes anything: see ADR 0012.
func latestVersion(cmd *cobra.Command, env *Env) (string, error) {
	resolved, err := env.Resolve()
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(resolved.Host, "/") + "/api/v1/cli/version"
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "beaver/"+Version)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		Data struct {
			Latest string `json:"latest"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("unreadable version response")
	}
	return body.Data.Latest, nil
}
