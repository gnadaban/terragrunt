package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gruntwork-io/terragrunt/config"
	"github.com/gruntwork-io/terragrunt/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/util"
	"github.com/hashicorp/go-getter"
)

// manifest for files coped from terragrunt module folder (i.e., the folder that contains the current terragrunt.hcl)
const MODULE_MANIFEST_NAME = ".terragrunt-module-manifest"

// 1. Download the given source URL, which should use Terraform's module source syntax, into a temporary folder
// 2. Copy the contents of terragruntOptions.WorkingDir into the temporary folder.
// 3. Set terragruntOptions.WorkingDir to the temporary folder.
//
// See the processTerraformSource method for how we determine the temporary folder so we can reuse it across multiple
// runs of Terragrunt to avoid downloading everything from scratch every time.
func downloadTerraformSource(source string, terragruntOptions *options.TerragruntOptions, terragruntConfig *config.TerragruntConfig) error {
	terraformSource, err := config.ProcessTerraformSource(source, terragruntOptions)
	if err != nil {
		return err
	}

	if err := downloadTerraformSourceIfNecessary(terraformSource, terragruntOptions, terragruntConfig); err != nil {
		return err
	}

	terragruntOptions.Logger.Printf("Copying files from %s into %s", terragruntOptions.WorkingDir, terraformSource.WorkingDir)
	if err := util.CopyFolderContents(terragruntOptions.WorkingDir, terraformSource.WorkingDir, MODULE_MANIFEST_NAME); err != nil {
		return err
	}

	terragruntOptions.Logger.Printf("Setting working directory to %s", terraformSource.WorkingDir)
	terragruntOptions.WorkingDir = terraformSource.WorkingDir

	return nil
}

// Download the specified TerraformSource if the latest code hasn't already been downloaded.
func downloadTerraformSourceIfNecessary(terraformSource *config.TerraformSource, terragruntOptions *options.TerragruntOptions, terragruntConfig *config.TerragruntConfig) error {
	if terragruntOptions.SourceUpdate {
		terragruntOptions.Logger.Printf("The --%s flag is set, so deleting the temporary folder %s before downloading source.", OPT_TERRAGRUNT_SOURCE_UPDATE, terraformSource.DownloadDir)
		if err := os.RemoveAll(terraformSource.DownloadDir); err != nil {
			return errors.WithStackTrace(err)
		}
	}

	alreadyLatest, err := alreadyHaveLatestCode(terraformSource, terragruntOptions)
	if err != nil {
		return err
	}

	if alreadyLatest {
		terragruntOptions.Logger.Printf("Terraform files in %s are up to date. Will not download again.", terraformSource.WorkingDir)
		return nil
	}

	// When downloading source, we need to process any hooks waiting on `init-from-module`. Therefore, we clone the
	// options struct, set the command to the value the hooks are expecting, and run the download action surrounded by
	// before and after hooks (if any).
	terragruntOptionsForDownload := terragruntOptions.Clone(terragruntOptions.TerragruntConfigPath)
	terragruntOptionsForDownload.TerraformCommand = CMD_INIT_FROM_MODULE
	downloadErr := runActionWithHooks("download source", terragruntOptionsForDownload, terragruntConfig, func() error {
		return downloadSource(terraformSource, terragruntOptions, terragruntConfig)
	})

	if downloadErr != nil {
		return downloadErr
	}

	if err := terraformSource.WriteVersionFile(); err != nil {
		return err
	}

	return nil
}

// Returns true if the specified TerraformSource, of the exact same version, has already been downloaded into the
// DownloadFolder. This helps avoid downloading the same code multiple times. Note that if the TerraformSource points
// to a local file path, we assume the user is doing local development and always return false to ensure the latest
// code is downloaded (or rather, copied) every single time. See the processTerraformSource method for more info.
func alreadyHaveLatestCode(terraformSource *config.TerraformSource, terragruntOptions *options.TerragruntOptions) (bool, error) {
	if config.IsLocalSource(terraformSource.CanonicalSourceURL) ||
		!util.FileExists(terraformSource.DownloadDir) ||
		!util.FileExists(terraformSource.WorkingDir) ||
		!util.FileExists(terraformSource.VersionFile) {

		return false, nil
	}

	tfFiles, err := filepath.Glob(fmt.Sprintf("%s/*.tf", terraformSource.WorkingDir))
	if err != nil {
		return false, errors.WithStackTrace(err)
	}

	if len(tfFiles) == 0 {
		terragruntOptions.Logger.Printf("Working dir %s exists but contains no Terraform files, so assuming code needs to be downloaded again.", terraformSource.WorkingDir)
		return false, nil
	}

	currentVersion := config.EncodeSourceVersion(terraformSource.CanonicalSourceURL)
	previousVersion, err := terraformSource.ReadVersionFile()

	if err != nil {
		return false, err
	}

	return previousVersion == currentVersion, nil
}

// We use this code to force go-getter to copy files instead of creating symlinks.
var copyFiles = func(client *getter.Client) error {
	// We copy all the default getters from the go-getter library, but replace the "file" getter. We shallow clone the
	// getter map here rather than using getter.Getters directly because (a) we shouldn't change the original,
	// globally-shared getter.Getters map and (b) Terragrunt may run this code from many goroutines concurrently during
	// xxx-all calls, so creating a new map each time ensures we don't a "concurrent map writes" error.
	client.Getters = map[string]getter.Getter{}
	for getterName, getterValue := range getter.Getters {
		if getterName == "file" {
			client.Getters[getterName] = &FileCopyGetter{}
		} else {
			client.Getters[getterName] = getterValue
		}
	}

	return nil
}

// Download the code from the Canonical Source URL into the Download Folder using the go-getter library
func downloadSource(terraformSource *config.TerraformSource, terragruntOptions *options.TerragruntOptions, terragruntConfig *config.TerragruntConfig) error {
	terragruntOptions.Logger.Printf("Downloading Terraform configurations from %s into %s", terraformSource.CanonicalSourceURL, terraformSource.DownloadDir)

	if err := getter.GetAny(terraformSource.DownloadDir, terraformSource.CanonicalSourceURL.String(), copyFiles); err != nil {
		return errors.WithStackTrace(err)
	}

	return nil
}

// There are two ways a user can tell Terragrunt that it needs to download Terraform configurations from a specific
// URL: via a command-line option or via an entry in the Terragrunt configuration. If the user used one of these, this
// method returns the source URL or an empty string if there is no source url
func getTerraformSourceUrl(terragruntOptions *options.TerragruntOptions, terragruntConfig *config.TerragruntConfig) string {
	if terragruntOptions.Source != "" {
		return terragruntOptions.Source
	} else if terragruntConfig.Terraform != nil && terragruntConfig.Terraform.Source != nil {
		return *terragruntConfig.Terraform.Source
	} else {
		return ""
	}
}
