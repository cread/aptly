package cmd

import (
	"fmt"
	"github.com/smira/aptly/aptly"
	"github.com/smira/aptly/deb"
	"github.com/smira/aptly/utils"
	"github.com/smira/commander"
	"github.com/smira/flag"
	"os"
	"path/filepath"
	"strings"
)

func aptlyRepoAdd(cmd *commander.Command, args []string) error {
	var err error
	if len(args) < 2 {
		cmd.Usage()
		return commander.ErrCommandError
	}

	name := args[0]

	verifier := &utils.GpgVerifier{}

	repo, err := context.CollectionFactory().LocalRepoCollection().ByName(name)
	if err != nil {
		return fmt.Errorf("unable to add: %s", err)
	}

	err = context.CollectionFactory().LocalRepoCollection().LoadComplete(repo)
	if err != nil {
		return fmt.Errorf("unable to add: %s", err)
	}

	context.Progress().Printf("Loading packages...\n")

	list, err := deb.NewPackageListFromRefList(repo.RefList(), context.CollectionFactory().PackageCollection(), context.Progress())
	if err != nil {
		return fmt.Errorf("unable to load packages: %s", err)
	}

	forceReplace := context.Flags().Lookup("force-replace").Value.Get().(bool)

	var packageFiles, failedFiles []string

	packageFiles, failedFiles, err = deb.CollectPackageFiles(args[1:], &aptly.ConsoleResultReporter{context.Progress()})
	if err != nil {
		return fmt.Errorf("unable to collect package files: %s", err)
	}

	processedFiles := []string{}

	if forceReplace {
		list.PrepareIndex()
	}

	for _, file := range packageFiles {
		var (
			stanza deb.Stanza
			p      *deb.Package
		)

		candidateProcessedFiles := []string{}
		isSourcePackage := strings.HasSuffix(file, ".dsc")
		isUdebPackage := strings.HasSuffix(file, ".udeb")

		if isSourcePackage {
			stanza, err = deb.GetControlFileFromDsc(file, verifier)

			if err == nil {
				stanza["Package"] = stanza["Source"]
				delete(stanza, "Source")

				p, err = deb.NewSourcePackageFromControlFile(stanza)
			}
		} else {
			stanza, err = deb.GetControlFileFromDeb(file)
			if isUdebPackage {
				p = deb.NewUdebPackageFromControlFile(stanza)
			} else {
				p = deb.NewPackageFromControlFile(stanza)
			}
		}
		if err != nil {
			context.Progress().ColoredPrintf("@y[!]@| @!Unable to read file %s: %s@|", file, err)
			failedFiles = append(failedFiles, file)
			continue
		}

		var checksums utils.ChecksumInfo
		checksums, err = utils.ChecksumsForFile(file)
		if err != nil {
			return err
		}

		if isSourcePackage {
			p.UpdateFiles(append(p.Files(), deb.PackageFile{Filename: filepath.Base(file), Checksums: checksums}))
		} else {
			p.UpdateFiles([]deb.PackageFile{deb.PackageFile{Filename: filepath.Base(file), Checksums: checksums}})
		}

		err = context.PackagePool().Import(file, checksums.MD5)
		if err != nil {
			context.Progress().ColoredPrintf("@y[!]@| @!Unable to import file %s into pool: %s@|", file, err)
			failedFiles = append(failedFiles, file)
			continue
		}

		candidateProcessedFiles = append(candidateProcessedFiles, file)

		// go over all files, except for the last one (.dsc/.deb itself)
		for _, f := range p.Files() {
			if filepath.Base(f.Filename) == filepath.Base(file) {
				continue
			}
			sourceFile := filepath.Join(filepath.Dir(file), filepath.Base(f.Filename))
			err = context.PackagePool().Import(sourceFile, f.Checksums.MD5)
			if err != nil {
				context.Progress().ColoredPrintf("@y[!]@| @!Unable to import file %s into pool: %s@|", sourceFile, err)
				failedFiles = append(failedFiles, file)
				break
			}

			candidateProcessedFiles = append(candidateProcessedFiles, sourceFile)
		}
		if err != nil {
			// some files haven't been imported
			continue
		}

		err = context.CollectionFactory().PackageCollection().Update(p)
		if err != nil {
			context.Progress().ColoredPrintf("@y[!]@| @!Unable to save package %s: %s@|", p, err)
			failedFiles = append(failedFiles, file)
			continue
		}

		if forceReplace {
			conflictingPackages := list.Search(deb.Dependency{Pkg: p.Name, Version: p.Version, Architecture: p.Architecture}, true)
			for _, cp := range conflictingPackages {
				context.Progress().ColoredPrintf("@r[-]@| %s removed due to conflict with package being added", cp)
				list.Remove(cp)
			}
		}

		err = list.Add(p)
		if err != nil {
			context.Progress().ColoredPrintf("@y[!]@| @!Unable to add package to repo %s: %s@|", p, err)
			failedFiles = append(failedFiles, file)
			continue
		}

		context.Progress().ColoredPrintf("@g[+]@| %s added@|", p)
		processedFiles = append(processedFiles, candidateProcessedFiles...)
	}

	repo.UpdateRefList(deb.NewPackageRefListFromPackageList(list))

	err = context.CollectionFactory().LocalRepoCollection().Update(repo)
	if err != nil {
		return fmt.Errorf("unable to save: %s", err)
	}

	if context.Flags().Lookup("remove-files").Value.Get().(bool) {
		processedFiles = utils.StrSliceDeduplicate(processedFiles)

		for _, file := range processedFiles {
			err := os.Remove(file)
			if err != nil {
				return fmt.Errorf("unable to remove file: %s", err)
			}
		}
	}

	if len(failedFiles) > 0 {
		context.Progress().ColoredPrintf("@y[!]@| @!Some files were skipped due to errors:@|")
		for _, file := range failedFiles {
			context.Progress().ColoredPrintf("  %s", file)
		}

		return fmt.Errorf("some files failed to be added")
	}

	return err
}

func makeCmdRepoAdd() *commander.Command {
	cmd := &commander.Command{
		Run:       aptlyRepoAdd,
		UsageLine: "add <name> <package file.deb>|<directory> ...",
		Short:     "add packages to local repository",
		Long: `
Command adds packages to local repository from .deb, .udeb (binary packages) and .dsc (source packages) files.
When importing from directory aptly would do recursive scan looking for all files matching *.[u]deb or *.dsc
patterns. Every file discovered would be analyzed to extract metadata, package would then be created and added
to the database. Files would be imported to internal package pool. For source packages, all required files are
added automatically as well. Extra files for source package should be in the same directory as *.dsc file.

Example:

  $ aptly repo add testing myapp-0.1.2.deb incoming/
`,
		Flag: *flag.NewFlagSet("aptly-repo-add", flag.ExitOnError),
	}

	cmd.Flag.Bool("remove-files", false, "remove files that have been imported successfully into repository")
	cmd.Flag.Bool("force-replace", false, "when adding package that conflicts with existing package, remove existing package")

	return cmd
}
