package command

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/client"
	"github.com/techbeaver/beaver-cli/internal/output"
	"github.com/techbeaver/beaver-cli/internal/sandboxerr"
)

const (
	// uploadChunkBytes is the most one PUT carries.
	uploadChunkBytes = 3 << 20
	// maxUploadBytes is the file size limit the API states for a sandbox file.
	maxUploadBytes = 100 << 20
	transferWait   = 5 * time.Minute
	workspaceRoot  = "/workspace"
)

var remoteRef = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}):(/.*)$`)

func parseRemote(arg string) (id, remotePath string, ok bool) {
	m := remoteRef.FindStringSubmatch(arg)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func newSandboxCopyCommand(env *Env) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "cp SOURCE DEST",
		Short: "Copy one file to or from a running sandbox",
		Long: "Copies one file. A sandbox path is ID:/workspace/path. Exactly one side is a sandbox path.\n\n" +
			"  beaver sandbox cp ./app.tar SANDBOX_ID:/workspace/app.tar\n" +
			"  beaver sandbox cp SANDBOX_ID:/workspace/out.log ./out.log\n\n" +
			"Uploads go in 3 MiB chunks and downloads are streamed, so size is not held in memory.\n" +
			"A download refuses to replace an existing local file unless you pass --force.\n" +
			"Remote paths must be absolute, inside /workspace and already normalised.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcID, srcPath, srcRemote := parseRemote(args[0])
			dstID, dstPath, dstRemote := parseRemote(args[1])
			switch {
			case srcRemote && dstRemote:
				return usageErr("copying between two sandboxes is not supported: copy down, then up")
			case !srcRemote && !dstRemote:
				return usageErr("one side must be a sandbox path like ID:/workspace/file")
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			if dstRemote {
				return uploadFile(cmd, env, s, args[0], dstID, dstPath)
			}
			return downloadFile(cmd, env, s, srcID, srcPath, args[1], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing local file when downloading")
	return cmd
}

func uploadFile(cmd *cobra.Command, env *Env, s *Session, local, id, remote string) error {
	f, err := os.Open(local) //nolint:gosec // the path is the caller's own argument
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", local, errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", local, err)
	}
	if !st.Mode().IsRegular() {
		return usageErr("%s is not a regular file: cp copies one file at a time", local)
	}
	size := st.Size()
	if size > maxUploadBytes {
		return usageErr("%s is %d bytes and sandbox files are limited to 100 MiB", local, size)
	}
	if strings.HasSuffix(remote, "/") {
		remote += filepath.Base(local)
	}
	long := s.Client.WithTimeout(transferWait)
	buf := make([]byte, uploadChunkBytes)
	var sent int64
	for first := true; ; first = false {
		n, rerr := io.ReadFull(f, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return fmt.Errorf("reading %s: %w", local, rerr)
		}
		last := n < len(buf) || sent+int64(n) >= size
		q := url.Values{"path": {remote}, "parents": {"true"}}
		if !first || !last {
			q.Set("offset", strconv.FormatInt(sent, 10))
		}
		req := client.Request{Method: http.MethodPut, Path: sandboxPath(id, "/files"), Query: q, RawBody: append([]byte{}, buf[:n]...)}
		_, err := retryOnce429(cmd.Context(), env, func() (*client.Envelope, error) {
			return long.Do(cmd.Context(), s.Token, req)
		})
		if err != nil {
			if sent > 0 {
				fmt.Fprintf(env.Err, "Upload stopped after %d of %d bytes. The remote file is incomplete: run the copy again to start over.\n", sent, size)
			}
			return sandboxerr.Describe(err)
		}
		sent += int64(n)
		if last {
			break
		}
	}
	return writeCopyResult(env, map[string]any{"direction": "upload", "sandboxId": id, "remotePath": remote, "localPath": local, "bytes": sent})
}

func downloadFile(cmd *cobra.Command, env *Env, s *Session, id, remote, local string, force bool) error {
	if st, err := os.Stat(local); err == nil && st.IsDir() {
		local = filepath.Join(local, path.Base(remote))
	}
	if _, err := os.Lstat(local); err == nil && !force {
		return usageErr("%s already exists: use --force to replace it", local)
	}
	long := s.Client.WithTimeout(transferWait)
	req := client.Request{Method: http.MethodGet, Path: sandboxPath(id, "/files"), Query: url.Values{"path": {remote}}}
	resp, err := retryOnce429(cmd.Context(), env, func() (*http.Response, error) {
		return long.Open(cmd.Context(), s.Token, req)
	})
	if err != nil {
		return sandboxerr.Describe(err)
	}
	defer func() { _ = resp.Body.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(local), ".beaver-cp-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", local, errors.Unwrap(err))
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	n, err := io.Copy(tmp, resp.Body)
	if err != nil {
		cleanup()
		return sandboxerr.Describe(fmt.Errorf("calling the TechBeaver API: the download broke off: %w", err))
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		cleanup()
		return fmt.Errorf("the download was cut short (%d of %d bytes), so nothing was written", n, resp.ContentLength)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", local, err)
	}
	if _, err := os.Lstat(local); err == nil && !force {
		_ = os.Remove(tmpName)
		return usageErr("%s already exists: use --force to replace it", local)
	}
	if err := os.Rename(tmpName, local); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", local, err)
	}
	return writeCopyResult(env, map[string]any{"direction": "download", "sandboxId": id, "remotePath": remote, "localPath": local, "bytes": n})
}

func writeCopyResult(env *Env, obj map[string]any) error {
	spec, _, err := env.outputSpec()
	if err != nil {
		return err
	}
	return output.New(env.Out, spec).Object(obj)
}

func remoteOrDefault(arg string, requirePath bool) (string, string, error) {
	if id, p, ok := parseRemote(arg); ok {
		return id, p, nil
	}
	if !requirePath && uuidPattern.MatchString(arg) {
		return arg, workspaceRoot, nil
	}
	return "", "", usageErr("expected a sandbox path like ID:/workspace/dir, got %q", arg)
}

func newSandboxLsCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "ls ID[:/path]",
		Short: "List a directory in a running sandbox",
		Long:  "Lists a directory inside /workspace. With only an id it lists /workspace.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, dir, err := remoteOrDefault(args[0], false)
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{
				Method: http.MethodGet, Path: sandboxPath(id, "/files/list"), Query: url.Values{"path": {dir}},
			})
			if err != nil {
				return err
			}
			obj, err := sandboxObject(envelope)
			if err != nil {
				return err
			}
			if obj["truncated"] == true {
				fmt.Fprintln(env.Err, "beaver: the listing was cut off at the API's limit.")
			}
			spec, human, err := env.outputSpec()
			if err != nil {
				return err
			}
			w := output.New(env.Out, spec)
			if !human {
				return w.Object(obj)
			}
			return w.List(asList(obj["entries"]), "type", "size", "modTime", "name")
		},
	}
}

func newSandboxMkdirCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir ID:/path",
		Short: "Create a directory, and its parents, in a running sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, dir, err := remoteOrDefault(args[0], true)
			if err != nil {
				return err
			}
			s, err := env.Authenticate(cmd.Context())
			if err != nil {
				return err
			}
			envelope, err := sandboxDo(env, s, cmd, client.Request{
				Method: http.MethodPost, Path: sandboxPath(id, "/files/mkdir"), Query: url.Values{"path": {dir}},
			})
			if err != nil {
				return err
			}
			return renderSandboxObject(env, envelope)
		},
	}
}
