// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package cmdeval

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	docs "github.com/GoogleContainerTools/kpt/internal/docs/generated/fndocs"
	"github.com/GoogleContainerTools/kpt/internal/fnruntime"
	"github.com/GoogleContainerTools/kpt/internal/pkg"
	"github.com/GoogleContainerTools/kpt/internal/printer"
	"github.com/GoogleContainerTools/kpt/internal/util/argutil"
	"github.com/GoogleContainerTools/kpt/internal/util/cmdutil"
	"github.com/GoogleContainerTools/kpt/internal/util/pathutil"
	kptfile "github.com/GoogleContainerTools/kpt/pkg/api/kptfile/v1"
	"github.com/GoogleContainerTools/kpt/pkg/kptfile/kptfileutil"
	"github.com/GoogleContainerTools/kpt/thirdparty/cmdconfig/commands/runner"
	"github.com/GoogleContainerTools/kpt/thirdparty/kyaml/runfn"
	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"sigs.k8s.io/kustomize/kyaml/errors"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/fn/runtime/runtimeutil"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// GetEvalFnRunner returns a EvalFnRunner.
func GetEvalFnRunner(ctx context.Context, parent string) *EvalFnRunner {
	r := &EvalFnRunner{Ctx: ctx}
	c := &cobra.Command{
		Use:     "eval [DIR | -] [flags] [--fn-args]",
		Short:   docs.EvalShort,
		Long:    docs.EvalShort + "\n" + docs.EvalLong,
		Example: docs.EvalExamples,
		RunE:    r.runE,
		PreRunE: r.preRunE,
	}

	r.Command = c
	r.Command.Flags().StringVarP(&r.Dest, "output", "o", "",
		fmt.Sprintf("output resources are written to provided location. Allowed values: %s|%s|<OUT_DIR_PATH>", cmdutil.Stdout, cmdutil.Unwrap))
	r.Command.Flags().StringVarP(
		&r.Image, "image", "i", "", "run this image as a function")
	_ = r.Command.RegisterFlagCompletionFunc("image", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return cmdutil.FetchFunctionImages(), cobra.ShellCompDirectiveDefault
	})
	r.Command.Flags().BoolVarP(
		&r.SaveFn, "save", "s", false,
		"save the function and its arguments to Kptfile")
	r.Command.Flags().StringVar(
		&r.Exec, "exec", "", "run an executable as a function")
	r.Command.Flags().StringVar(
		&r.FnConfigPath, "fn-config", "", "path to the function config file")
	r.Command.Flags().BoolVarP(
		&r.IncludeMetaResources, "include-meta-resources", "m", false, "include package meta resources in function input")
	r.Command.Flags().StringVar(
		&r.ResultsDir, "results-dir", "", "write function results to this dir")
	r.Command.Flags().BoolVar(
		&r.Network, "network", false, "enable network access for functions that declare it")
	r.Command.Flags().StringArrayVar(
		&r.Mounts, "mount", []string{},
		"a list of storage options read from the filesystem")
	r.Command.Flags().StringArrayVarP(
		&r.Env, "env", "e", []string{},
		"a list of environment variables to be used by functions")
	r.Command.Flags().BoolVar(
		&r.AsCurrentUser, "as-current-user", false, "use the uid and gid that kpt is running with to run the function in the container")
	r.Command.Flags().StringVar(&r.ImagePullPolicy, "image-pull-policy", string(fnruntime.IfNotPresentPull),
		fmt.Sprintf("pull image before running the container. It must be one of %s, %s and %s.", fnruntime.AlwaysPull, fnruntime.IfNotPresentPull, fnruntime.NeverPull))
	r.Command.Flags().StringVar(
		&r.Selector.APIVersion, "match-api-version", "", "select resources matching the given apiVersion")
	r.Command.Flags().StringVar(
		&r.Selector.Kind, "match-kind", "", "select resources matching the given kind")
	r.Command.Flags().StringVar(
		&r.Selector.Name, "match-name", "", "select resources matching the given name")
	r.Command.Flags().StringVar(
		&r.Selector.Namespace, "match-namespace", "", "select resources matching the given namespace")

	cmdutil.FixDocs("kpt", parent, c)
	return r
}

func EvalCommand(ctx context.Context, name string) *cobra.Command {
	return GetEvalFnRunner(ctx, name).Command
}

// EvalFnRunner contains the run function
type EvalFnRunner struct {
	Command              *cobra.Command
	Dest                 string
	OutContent           bytes.Buffer
	FromStdin            bool
	Image                string
	SaveFn               bool
	Exec                 string
	FnConfigPath         string
	RunFns               runfn.RunFns
	ResultsDir           string
	ImagePullPolicy      string
	Network              bool
	Mounts               []string
	Env                  []string
	AsCurrentUser        bool
	IncludeMetaResources bool
	Ctx                  context.Context
	Selector             kptfile.Selector
	dataItems            []string
}

func (r *EvalFnRunner) runE(c *cobra.Command, _ []string) error {
	err := runner.HandleError(r.Ctx, r.RunFns.Execute())
	if err != nil {
		return err
	}
	if err = cmdutil.WriteFnOutput(r.Dest, r.OutContent.String(), r.FromStdin,
		printer.FromContextOrDie(r.Ctx).OutStream()); err != nil {
		return err
	}
	if r.SaveFn {
		r.SaveFnToKptfile()
	}
	return nil
}

// NewFunction creates a Kptfile.Function object which has the evaluated fn configurations.
// This object can be written to Kptfile `pipeline.mutators`.
func (r *EvalFnRunner) NewFunction() *kptfile.Function {
	newFn := &kptfile.Function{}
	if r.Image != "" {
		newFn.Image = r.Image
		newFn.Name = r.Image
	} else {
		newFn.Exec = r.Exec
		newFn.Name = r.Exec
	}
	if !r.Selector.IsEmpty() {
		newFn.Selectors = []kptfile.Selector{r.Selector}
	}
	if r.FnConfigPath != "" {
		_, relativePath, _ := pathutil.ResolveAbsAndRelPaths(r.FnConfigPath)
		newFn.ConfigPath = relativePath
	} else {
		data := map[string]string{}
		for i, s := range r.dataItems {
			kv := strings.SplitN(s, "=", 2)
			if i == 0 && len(kv) == 1 {
				continue
			}
			data[kv[0]] = kv[1]
		}
		if len(data) != 0 {
			newFn.ConfigMap = data
		}
	}
	return newFn
}

// SaveFnToKptfile adds the evaluated function and its arguments to Kptfile `pipeline.mutators` list.
func (r *EvalFnRunner) SaveFnToKptfile() {
	pr := printer.FromContextOrDie(r.Ctx)
	kf, err := pkg.ReadKptfile(filesys.FileSystemOrOnDisk{}, r.Dest)
	if err != nil {
		pr.Printf("function not added: Kptfile not exists\n")
		return
	}
	// TODO(yuwenma): Right now we cannot tell if a function is a mutator or validator. Once kpt supports
	// OCI images, we can add annotations to image and find out the function type from these annotations.
	if kf.Pipeline == nil {
		kf.Pipeline = &kptfile.Pipeline{}
	}
	if kf.Pipeline.Mutators == nil {
		kf.Pipeline.Mutators = []kptfile.Function{}
	} else {
		for _, m := range kf.Pipeline.Mutators {
			if m.Name == r.Image || m.Image == r.Image {
				pr.Printf("skip adding image: already exists in Kptfile\n")
				return
			}
			if m.Name == r.Exec || m.Exec == r.Exec {
				pr.Printf("skip adding exec: already exists in Kptfile\n")
				return
			}
		}
	}
	kf.Pipeline.Mutators = append(kf.Pipeline.Mutators, *r.NewFunction())
	if err = kptfileutil.WriteFile(r.Dest, kf); err != nil {
		pr.Printf("function is not added to Kptfile: %v\n", err)
		return
	}
	pr.Printf("function is added to Kptfile\n")
}

// getCLIFunctionConfig parses the commandline flags and arguments into explicit
// function config
func (r *EvalFnRunner) getCLIFunctionConfig(dataItems []string) (
	*yaml.RNode, error) {

	if r.Image == "" && r.Exec == "" {
		return nil, nil
	}

	var err error

	// create the function config
	rc, err := yaml.Parse(`
metadata:
  name: function-input
data: {}
`)
	if err != nil {
		return nil, err
	}

	// default the function config kind to ConfigMap, this may be overridden
	var kind = "ConfigMap"
	var version = "v1"

	// populate the function config with data.  this is a convention for functions
	// to be more commandline friendly
	if len(dataItems) > 0 {
		dataField, err := rc.Pipe(yaml.Lookup("data"))
		if err != nil {
			return nil, err
		}
		for i, s := range dataItems {
			kv := strings.SplitN(s, "=", 2)
			if i == 0 && len(kv) == 1 {
				// first argument may be the kind
				kind = s
				continue
			}
			if len(kv) != 2 {
				return nil, fmt.Errorf("args must have keys and values separated by =")
			}
			err := dataField.PipeE(yaml.SetField(kv[0], yaml.NewScalarRNode(kv[1])))
			if err != nil {
				return nil, err
			}
		}
	}
	err = rc.PipeE(yaml.SetField("kind", yaml.NewScalarRNode(kind)))
	if err != nil {
		return nil, err
	}
	err = rc.PipeE(yaml.SetField("apiVersion", yaml.NewScalarRNode(version)))
	if err != nil {
		return nil, err
	}
	return rc, nil
}

func (r *EvalFnRunner) getFunctionSpec() (*runtimeutil.FunctionSpec, []string, error) {
	fn := &runtimeutil.FunctionSpec{}
	var execArgs []string
	if r.Image != "" {
		if err := kptfile.ValidateFunctionImageURL(r.Image); err != nil {
			return nil, nil, err
		}
		fn.Container.Image = r.Image
	} else if r.Exec != "" {
		// check the flags that doesn't make sense with exec function
		// --mount, --as-current-user, --network and --env are
		// only used with container functions
		if r.AsCurrentUser || r.Network ||
			len(r.Mounts) != 0 || len(r.Env) != 0 {
			return nil, nil, fmt.Errorf("--mount, --as-current-user, --network and --env can only be used with container functions")
		}
		s, err := shlex.Split(r.Exec)
		if err != nil {
			return nil, nil, fmt.Errorf("exec command %q must be valid: %w", r.Exec, err)
		}
		if len(s) > 0 {
			fn.Exec.Path = s[0]
			execArgs = s[1:]
		}

	}
	return fn, execArgs, nil
}

func toStorageMounts(mounts []string) []runtimeutil.StorageMount {
	var sms []runtimeutil.StorageMount
	for _, mount := range mounts {
		sms = append(sms, runtimeutil.StringToStorageMount(mount))
	}
	return sms
}

func checkFnConfigPathExistence(path string) error {
	// check does fn config file exist
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("missing function config file: %s", path)
	}
	return nil
}

func (r *EvalFnRunner) preRunE(c *cobra.Command, args []string) error {
	if r.Dest != "" && r.Dest != cmdutil.Stdout && r.Dest != cmdutil.Unwrap {
		if err := cmdutil.CheckDirectoryNotPresent(r.Dest); err != nil {
			return err
		}
	}

	if r.Image == "" && r.Exec == "" {
		return errors.Errorf("must specify --image or --exec")
	}
	if r.Image != "" {
		r.Image = fnruntime.AddDefaultImagePathPrefix(r.Image)
		err := cmdutil.DockerCmdAvailable()
		if err != nil {
			return err
		}
	}
	if err := cmdutil.ValidateImagePullPolicyValue(r.ImagePullPolicy); err != nil {
		return err
	}
	if r.ResultsDir != "" {
		err := os.MkdirAll(r.ResultsDir, 0755)
		if err != nil {
			return fmt.Errorf("cannot read or create results dir %q: %w", r.ResultsDir, err)
		}
	}
	var dataItems []string
	if c.ArgsLenAtDash() >= 0 {
		dataItems = append(dataItems, args[c.ArgsLenAtDash():]...)
		args = args[:c.ArgsLenAtDash()]
	}
	if len(args) == 0 {
		// default to current working directory
		args = append(args, ".")
	}
	if len(args) > 1 {
		return errors.Errorf("0 or 1 arguments supported, function arguments go after '--'")
	}
	if len(dataItems) > 0 && r.FnConfigPath != "" {
		return fmt.Errorf("function arguments can only be specified without function config file")
	}
	fnConfig, err := r.getCLIFunctionConfig(dataItems)
	if err != nil {
		return err
	}
	r.dataItems = dataItems
	fnSpec, execArgs, err := r.getFunctionSpec()
	if err != nil {
		return err
	}

	// set the output to stdout if in dry-run mode or no arguments are specified
	var output io.Writer
	var input io.Reader
	r.OutContent = bytes.Buffer{}
	if args[0] == "-" {
		output = &r.OutContent
		input = c.InOrStdin()
		r.FromStdin = true

		// clear args as it indicates stdin and not path
		args = []string{}
	} else if r.Dest != "" {
		output = &r.OutContent
	}

	// set the path if specified as an argument
	var path string
	if len(args) == 1 {
		// argument is the directory
		path = args[0]
	}

	// parse mounts to set storageMounts
	storageMounts := toStorageMounts(r.Mounts)

	if r.FnConfigPath != "" {
		err = checkFnConfigPathExistence(r.FnConfigPath)
		if err != nil {
			return err
		}
	}

	if path != "" {
		path, err = argutil.ResolveSymlink(r.Ctx, path)
		if err != nil {
			return err
		}
	}

	r.RunFns = runfn.RunFns{
		Ctx:                  r.Ctx,
		Function:             fnSpec,
		ExecArgs:             execArgs,
		OriginalExec:         r.Exec,
		Output:               output,
		Input:                input,
		Path:                 path,
		Network:              r.Network,
		StorageMounts:        storageMounts,
		ResultsDir:           r.ResultsDir,
		Env:                  r.Env,
		AsCurrentUser:        r.AsCurrentUser,
		FnConfig:             fnConfig,
		FnConfigPath:         r.FnConfigPath,
		IncludeMetaResources: r.IncludeMetaResources,
		ImagePullPolicy:      cmdutil.StringToImagePullPolicy(r.ImagePullPolicy),
		// fn eval should remove all files when all resources
		// are deleted.
		ContinueOnEmptyResult: true,
		Selector:              r.Selector,
	}

	return nil
}
