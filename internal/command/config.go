package command

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"github.com/techbeaver/beaver-cli/internal/config"
)

func newConfigCommand(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Read and change settings"}
	cmd.AddCommand(newConfigSet(env), newConfigGet(env), newConfigList(env), newConfigUseProfile(env))
	return cmd
}

var settableKeys = []string{"host", "project", "app"}

func newConfigSet(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Set host, project or app on the current profile",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			resolved, err := env.Resolve()
			if err != nil {
				return err
			}
			file, err := env.Loader.Load()
			if err != nil {
				return err
			}
			profile := file.Profiles[resolved.Profile]
			switch key {
			case "host":
				profile.Host = value
			case "project":
				profile.Project = value
			case "app":
				profile.App = value
			default:
				return usageErr("unknown setting %q: use one of %v", key, settableKeys)
			}
			file.Profiles[resolved.Profile] = profile
			if file.Current == "" {
				file.Current = resolved.Profile
			}
			if err := env.Loader.Save(file); err != nil {
				return err
			}
			fmt.Fprintf(env.Err, "Set %s on profile %q.\n", key, resolved.Profile)
			return nil
		},
	}
}

func newConfigGet(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "get KEY",
		Short: "Show one effective setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			resolved, err := env.Resolve()
			if err != nil {
				return err
			}
			var value string
			switch args[0] {
			case "host":
				value = resolved.Host
			case "project":
				value = resolved.Project
			case "app":
				value = resolved.App
			case "profile":
				value = resolved.Profile
			default:
				return usageErr("unknown setting %q: use one of %v", args[0], append(settableKeys, "profile"))
			}
			fmt.Fprintln(env.Out, value)
			return nil
		},
	}
}

func newConfigList(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show the effective settings and every profile",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			resolved, err := env.Resolve()
			if err != nil {
				return err
			}
			file, err := env.Loader.Load()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(file.Profiles))
			for name := range file.Profiles {
				names = append(names, name)
			}
			sort.Strings(names)

			_, projectPath, err := env.Loader.FindProjectFile()
			if err != nil {
				return err
			}

			w, err := env.Writer()
			if err != nil {
				return err
			}
			return w.Object(map[string]any{
				"profile":      resolved.Profile,
				"host":         resolved.Host,
				"project":      resolved.Project,
				"app":          resolved.App,
				"profiles":     names,
				"project_file": projectPath,
			})
		},
	}
}

func newConfigUseProfile(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "use-profile NAME",
		Short: "Make a profile the default for later commands",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			file, err := env.Loader.Load()
			if err != nil {
				return err
			}
			if _, ok := file.Profiles[args[0]]; !ok {
				file.Profiles[args[0]] = config.Profile{}
			}
			file.Current = args[0]
			if err := env.Loader.Save(file); err != nil {
				return err
			}
			fmt.Fprintf(env.Err, "Now using profile %q.\n", args[0])
			return nil
		},
	}
}
