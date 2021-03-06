package command

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	getter "github.com/hashicorp/go-getter"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform/backend"
	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/helper/variables"
	"github.com/hashicorp/terraform/plugin"
	"github.com/hashicorp/terraform/plugin/discovery"
	"github.com/hashicorp/terraform/terraform"
)

// InitCommand is a Command implementation that takes a Terraform
// module and clones it to the working directory.
type InitCommand struct {
	Meta

	// getProvider fetches providers that aren't found locally, and unpacks
	// them into the dst directory.
	// This uses discovery.GetProvider by default, but it provided here as a
	// way to mock fetching providers for tests.
	getProvider func(dst, provider string, req discovery.Constraints, protoVersion uint) error
}

func (c *InitCommand) Run(args []string) int {
	var flagBackend, flagGet, flagGetPlugins bool
	var flagConfigExtra map[string]interface{}

	args = c.Meta.process(args, false)
	cmdFlags := c.flagSet("init")
	cmdFlags.BoolVar(&flagBackend, "backend", true, "")
	cmdFlags.Var((*variables.FlagAny)(&flagConfigExtra), "backend-config", "")
	cmdFlags.BoolVar(&flagGet, "get", true, "")
	cmdFlags.BoolVar(&flagGetPlugins, "get-plugins", true, "")
	cmdFlags.BoolVar(&c.forceInitCopy, "force-copy", false, "suppress prompts about copying state data")
	cmdFlags.BoolVar(&c.Meta.stateLock, "lock", true, "lock state")
	cmdFlags.DurationVar(&c.Meta.stateLockTimeout, "lock-timeout", 0, "lock timeout")
	cmdFlags.BoolVar(&c.reconfigure, "reconfigure", false, "reconfigure")

	cmdFlags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	// set getProvider if we don't have a test version already
	if c.getProvider == nil {
		c.getProvider = discovery.GetProvider
	}

	// Validate the arg count
	args = cmdFlags.Args()
	if len(args) > 2 {
		c.Ui.Error("The init command expects at most two arguments.\n")
		cmdFlags.Usage()
		return 1
	}

	// Get our pwd. We don't always need it but always getting it is easier
	// than the logic to determine if it is or isn't needed.
	pwd, err := os.Getwd()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error getting pwd: %s", err))
		return 1
	}

	// Get the path and source module to copy
	var path string
	var source string
	switch len(args) {
	case 0:
		path = pwd
	case 1:
		path = pwd
		source = args[0]
	case 2:
		source = args[0]
		path = args[1]
	default:
		panic("assertion failed on arg count")
	}

	// Set the state out path to be the path requested for the module
	// to be copied. This ensures any remote states gets setup in the
	// proper directory.
	c.Meta.dataDir = filepath.Join(path, DefaultDataDir)

	// This will track whether we outputted anything so that we know whether
	// to output a newline before the success message
	var header bool

	// If we have a source, copy it
	if source != "" {
		c.Ui.Output(c.Colorize().Color(fmt.Sprintf(
			"[reset][bold]"+
				"Initializing configuration from: %q...", source)))
		if err := c.copySource(path, source, pwd); err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error copying source: %s", err))
			return 1
		}

		header = true
	}

	// If our directory is empty, then we're done. We can't get or setup
	// the backend with an empty directory.
	if empty, err := config.IsEmptyDir(path); err != nil {
		c.Ui.Error(fmt.Sprintf(
			"Error checking configuration: %s", err))
		return 1
	} else if empty {
		c.Ui.Output(c.Colorize().Color(strings.TrimSpace(outputInitEmpty)))
		return 0
	}

	var back backend.Backend

	// If we're performing a get or loading the backend, then we perform
	// some extra tasks.
	if flagGet || flagBackend {
		conf, err := c.Config(path)
		if err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error loading configuration: %s", err))
			return 1
		}

		// If we requested downloading modules and have modules in the config
		if flagGet && len(conf.Modules) > 0 {
			header = true

			c.Ui.Output(c.Colorize().Color(fmt.Sprintf(
				"[reset][bold]" +
					"Downloading modules (if any)...")))
			if err := getModules(&c.Meta, path, module.GetModeGet); err != nil {
				c.Ui.Error(fmt.Sprintf(
					"Error downloading modules: %s", err))
				return 1
			}

		}

		// If we're requesting backend configuration or looking for required
		// plugins, load the backend
		if flagBackend || flagGetPlugins {
			header = true

			// Only output that we're initializing a backend if we have
			// something in the config. We can be UNSETTING a backend as well
			// in which case we choose not to show this.
			if conf.Terraform != nil && conf.Terraform.Backend != nil {
				c.Ui.Output(c.Colorize().Color(fmt.Sprintf(
					"[reset][bold]" +
						"Initializing the backend...")))
			}

			opts := &BackendOpts{
				Config:      conf,
				ConfigExtra: flagConfigExtra,
				Init:        true,
			}
			if back, err = c.Backend(opts); err != nil {
				c.Ui.Error(err.Error())
				return 1
			}
		}
	}

	// Now that we have loaded all modules, check the module tree for missing providers
	if flagGetPlugins {
		sMgr, err := back.State(c.Env())
		if err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error loading state: %s", err))
			return 1
		}

		if err := sMgr.RefreshState(); err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error refreshing state: %s", err))
			return 1
		}

		c.Ui.Output(c.Colorize().Color(
			"[reset][bold]Initializing provider plugins...",
		))

		err = c.getProviders(path, sMgr.State())
		if err != nil {
			// this function provides its own output
			log.Printf("[ERROR] %s", err)
			return 1
		}
	}

	// If we outputted information, then we need to output a newline
	// so that our success message is nicely spaced out from prior text.
	if header {
		c.Ui.Output("")
	}

	c.Ui.Output(c.Colorize().Color(strings.TrimSpace(outputInitSuccess)))

	return 0
}

// Load the complete module tree, and fetch any missing providers.
// This method outputs its own Ui.
func (c *InitCommand) getProviders(path string, state *terraform.State) error {
	mod, err := c.Module(path)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error getting plugins: %s", err))
		return err
	}

	if err := mod.Validate(); err != nil {
		c.Ui.Error(fmt.Sprintf("Error getting plugins: %s", err))
		return err
	}

	available := c.providerPluginSet()
	requirements := terraform.ModuleTreeDependencies(mod, state).AllPluginRequirements()
	missing := c.missingPlugins(available, requirements)

	dst := c.pluginDir()
	var errs error
	for provider, reqd := range missing {
		c.Ui.Output(fmt.Sprintf("- downloading plugin for provider %q...", provider))
		err := c.getProvider(dst, provider, reqd.Versions, plugin.Handshake.ProtocolVersion)

		if err != nil {
			c.Ui.Error(fmt.Sprintf(errProviderNotFound, err, provider, reqd.Versions))
			errs = multierror.Append(errs, err)
		}
	}

	if errs != nil {
		return errs
	}

	// With all the providers downloaded, we'll generate our lock file
	// that ensures the provider binaries remain unchanged until we init
	// again. If anything changes, other commands that use providers will
	// fail with an error instructing the user to re-run this command.
	available = c.providerPluginSet() // re-discover to see newly-installed plugins
	chosen := choosePlugins(available, requirements)
	digests := map[string][]byte{}
	for name, meta := range chosen {
		digest, err := meta.SHA256()
		if err != nil {
			c.Ui.Error(fmt.Sprintf("failed to read provider plugin %s: %s", meta.Path, err))
			return err
		}
		digests[name] = digest
	}
	err = c.providerPluginsLock().Write(digests)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("failed to save provider manifest: %s", err))
		return err
	}

	// If any providers have "floating" versions (completely unconstrained)
	// we'll suggest the user constrain with a pessimistic constraint to
	// avoid implicitly adopting a later major release.
	constraintSuggestions := make(map[string]discovery.ConstraintStr)
	for name, meta := range chosen {
		req := requirements[name]
		if req == nil {
			// should never happen, but we don't want to crash here, so we'll
			// be cautious.
			continue
		}

		if req.Versions.Unconstrained() {
			// meta.Version.MustParse is safe here because our "chosen" metas
			// were already filtered for validity of versions.
			constraintSuggestions[name] = meta.Version.MustParse().MinorUpgradeConstraintStr()
		}
	}
	if len(constraintSuggestions) != 0 {
		names := make([]string, 0, len(constraintSuggestions))
		for name := range constraintSuggestions {
			names = append(names, name)
		}
		sort.Strings(names)

		c.Ui.Output(outputInitProvidersUnconstrained)
		for _, name := range names {
			c.Ui.Output(fmt.Sprintf("* provider.%s: version = %q", name, constraintSuggestions[name]))
		}
	}

	return nil
}

func (c *InitCommand) copySource(dst, src, pwd string) error {
	// Verify the directory is empty
	if empty, err := config.IsEmptyDir(dst); err != nil {
		return fmt.Errorf("Error checking on destination path: %s", err)
	} else if !empty {
		return fmt.Errorf(strings.TrimSpace(errInitCopyNotEmpty))
	}

	// Detect
	source, err := getter.Detect(src, pwd, getter.Detectors)
	if err != nil {
		return fmt.Errorf("Error with module source: %s", err)
	}

	// Get it!
	return module.GetCopy(dst, source)
}

func (c *InitCommand) Help() string {
	helpText := `
Usage: terraform init [options] [SOURCE] [PATH]

  Initialize a new or existing Terraform working directory by creating
  initial files, loading any remote state, downloading modules, etc.

  This is the first command that should be run for any new or existing
  Terraform configuration per machine. This sets up all the local data
  necessary to run Terraform that is typically not committed to version
  control.

  This command is always safe to run multiple times. Though subsequent runs
  may give errors, this command will never delete your configuration or
  state. Even so, if you have important information, please back it up prior
  to running this command, just in case.

  If no arguments are given, the configuration in this working directory
  is initialized.

  If one or two arguments are given, the first is a SOURCE of a module to
  download to the second argument PATH. After downloading the module to PATH,
  the configuration will be initialized as if this command were called pointing
  only to that PATH. PATH must be empty of any Terraform files. Any
  conflicting non-Terraform files will be overwritten. The module download
  is a copy. If you're downloading a module from Git, it will not preserve
  Git history.

Options:

  -backend=true        Configure the backend for this configuration.

  -backend-config=path This can be either a path to an HCL file with key/value
                       assignments (same format as terraform.tfvars) or a
                       'key=value' format. This is merged with what is in the
                       configuration file. This can be specified multiple
                       times. The backend type must be in the configuration
                       itself.

  -force-copy          Suppress prompts about copying state data. This is
                       equivalent to providing a "yes" to all confirmation
                       prompts.

  -get=true            Download any modules for this configuration.

  -get-plugins=true    Download any missing plugins for this configuration.

  -input=true          Ask for input if necessary. If false, will error if
                       input was required.

  -lock=true           Lock the state file when locking is supported.

  -lock-timeout=0s     Duration to retry a state lock.

  -no-color            If specified, output won't contain any color.

  -reconfigure          Reconfigure the backend, ignoring any saved configuration.
`
	return strings.TrimSpace(helpText)
}

func (c *InitCommand) Synopsis() string {
	return "Initialize a new or existing Terraform configuration"
}

const errInitCopyNotEmpty = `
The destination path contains Terraform configuration files. The init command
with a SOURCE parameter can only be used on a directory without existing
Terraform files.

Please resolve this issue and try again.
`

const outputInitEmpty = `
[reset][bold]Terraform initialized in an empty directory![reset]

The directory has no Terraform configuration files. You may begin working
with Terraform immediately by creating Terraform configuration files.
`

const outputInitSuccess = `
[reset][bold][green]Terraform has been successfully initialized![reset][green]

You may now begin working with Terraform. Try running "terraform plan" to see
any changes that are required for your infrastructure. All Terraform commands
should now work.

If you ever set or change modules or backend configuration for Terraform,
rerun this command to reinitialize your working directory. If you forget, other
commands will detect it and remind you to do so if necessary.
`

const outputInitProvidersUnconstrained = `
The following providers do not have any version constraints in configuration,
so the latest version was installed.

To prevent automatic upgrades to new major versions that may contain breaking
changes, it is recommended to add version = "..." constraints to the
corresponding provider blocks in configuration, with the constraint strings
suggested below.
`

const errProviderNotFound = `
[reset][red]%[1]s

[reset][bold][red]Error: Satisfying %[2]q, provider not found

[reset][red]A version of the %[2]q provider that satisfies all version
constraints could not be found. The requested version
constraints are shown below.

%[2]s = %[3]q[reset]
`
