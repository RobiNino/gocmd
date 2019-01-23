package dependencies

import (
	"fmt"
	"github.com/jfrog/gocmd/utils"
	"github.com/jfrog/gocmd/utils/cache"
	"github.com/jfrog/gocmd/utils/cmd"
	"github.com/jfrog/jfrog-client-go/artifactory"
	"github.com/jfrog/jfrog-client-go/artifactory/auth"
	"github.com/jfrog/jfrog-client-go/httpclient"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Represents go dependency when running with deps-tidy set to true.
type PackageWithDeps struct {
	Dependency             *Package
	transitiveDependencies []PackageWithDeps
	regExp                 *RegExp
	runGoModCommand        bool
	shouldRevertToEmptyMod bool
	TidyEnum               utils.TidyEnum
	cachePath              string
	GoModEditMessage       string
	originalModContent     []byte
}

// Creates a new dependency
func (pwd *PackageWithDeps) New(cachePath string, dependency Package) GoPackage {
	pwd.Dependency = &dependency
	pwd.cachePath = cachePath
	pwd.transitiveDependencies = nil
	return pwd
}

// Populate the mod file and publish the dependency and it's transitive dependencies to Artifactory
func (pwd *PackageWithDeps) PopulateModAndPublish(targetRepo string, cache *cache.DependenciesCache, serviceManager *artifactory.ArtifactoryServicesManager) error {
	var path string
	log.Debug("Starting to work on", pwd.Dependency.GetId())
	serviceManager.GetConfig().GetArtDetails()
	dependenciesMap := cache.GetMap()
	published, _ := dependenciesMap[pwd.Dependency.GetId()]
	if published {
		log.Debug("Overwriting the mod file in the cache from the one from Artifactory", pwd.Dependency.GetId())
		moduleAndVersion := strings.Split(pwd.Dependency.GetId(), ":")
		path = downloadModFileFromArtifactoryToLocalCache(pwd.cachePath, targetRepo, moduleAndVersion[0], moduleAndVersion[1], serviceManager.GetConfig().GetArtDetails(), httpclient.NewDefaultHttpClient())
		err := pwd.updateModContent(path, cache)
		logError(err)
	}

	// Checks if mod is empty, need to run go mod tidy command to populate the mod file.
	log.Debug(fmt.Sprintf("Dependency %s mod file is empty: %t", pwd.Dependency.GetId(), !pwd.PatternMatched(pwd.regExp.GetNotEmptyModRegex())))

	// Creates the dependency in the temp folder and runs go commands: go mod tidy and go mod graph.
	// Returns the path to the project in the temp and the a map with the project dependencies
	path, output, err := pwd.createDependencyAndPrepareMod(cache)
	logError(err)
	pwd.publishDependencyAndPopulateTransitive(path, targetRepo, output, cache, serviceManager)
	return nil
}

// Updating the new mod content
func (pwd *PackageWithDeps) updateModContent(path string, cache *cache.DependenciesCache) error {
	if path != "" {
		modContent, err := ioutil.ReadFile(path)
		if err != nil {
			cache.IncrementFailures()
			return errorutils.CheckError(err)
		}
		pwd.Dependency.SetModContent(modContent)
	}
	return nil
}

// Init the dependency information if needed.
func (pwd *PackageWithDeps) Init() error {
	var err error
	pwd.regExp, err = GetRegex()
	if err != nil {
		return err
	}
	return nil
}

// Returns true if regex found a match otherwise false.
func (pwd *PackageWithDeps) PatternMatched(regExp *regexp.Regexp) bool {
	lines := strings.Split(string(pwd.Dependency.modContent), "\n")
	for _, line := range lines {
		if regExp.FindString(line) != "" {
			return true
		}
	}
	return false
}

// Creates the dependency in the temp folder and runs go mod tidy and go mod graph
// Returns the path to the project in the temp and the a map with the project dependencies
func (pwd *PackageWithDeps) createDependencyAndPrepareMod(cache *cache.DependenciesCache) (path string, output map[string]bool, err error) {
	path, err = pwd.getModPathAndUnzipDependency(path)
	if err != nil {
		return
	}
	pwd.shouldRevertToEmptyMod = false
	// Check the mod in the cache if empty or not
	if pwd.PatternMatched(pwd.regExp.GetNotEmptyModRegex()) {
		err = pwd.useCachedMod(path)
		if err != nil {
			return
		}
	} else {
		published, _ := cache.GetMap()[pwd.Dependency.GetId()]
		var originalModContent []byte
		if !published {
			originalModContent = pwd.prepareUnpublishedDependency(path, originalModContent)
		} else {
			originalModContent = pwd.Dependency.GetModContent()
			// Put the mod file to temp
			err = writeModContentToModFile(path, pwd.Dependency.GetModContent())
			logError(err)
		}
		// If not empty --> use the mod file and don't run go mod tidy
		// If empty --> Run go mod tidy. Publish the package with empty mod file.
		if !pwd.PatternMatched(pwd.regExp.GetNotEmptyModRegex()) {
			log.Debug("The mod still empty after running 'go mod init' for:", pwd.Dependency.GetId())
			err = populateModWithTidy(path)
			logError(err)
			// Need to remember here to revert to the empty mod file.
			pwd.shouldRevertToEmptyMod = true
			pwd.originalModContent = originalModContent
		} else {
			log.Debug("Project mod file after init is not empty", pwd.Dependency.id)
		}
	}
	output, err = runGoModGraph()
	return
}

func (pwd *PackageWithDeps) prepareUnpublishedDependency(pathToModFile string, originalModContent []byte) []byte {
	err := pwd.prepareAndRunInit(pathToModFile)
	if err != nil {
		log.Error(err)
		exists, err := fileutils.IsFileExists(pathToModFile, false)
		logError(err)
		if !exists {
			// Create a mod file
			err = writeModContentToModFile(pathToModFile, pwd.Dependency.GetModContent())
			logError(err)
		}
	}
	// Got here means init worked or mod was created. Need to check the content if mod is empty or not
	modContent, err := ioutil.ReadFile(pathToModFile)
	logError(err)
	originalModContent = pwd.Dependency.GetModContent()
	pwd.Dependency.SetModContent(modContent)
	return originalModContent
}

func (pwd *PackageWithDeps) useCachedMod(path string) error {
	// Mod not empty in the cache. Use it.
	log.Debug("Using the mod in the cache since not empty:", pwd.Dependency.GetId())
	err := writeModContentToModFile(path, pwd.Dependency.GetModContent())
	logError(err)
	err = os.Chdir(filepath.Dir(path))
	if errorutils.CheckError(err) != nil {
		return err
	}
	logError(removeGoSum(path))
	return nil
}

func (pwd *PackageWithDeps) getModPathAndUnzipDependency(path string) (string, error) {
	err := os.Unsetenv(cmd.GOPROXY)
	if err != nil {
		return "", err
	}
	// Unzips the zip file into temp
	tempDir, err := createDependencyInTemp(pwd.Dependency.GetZipPath())
	if err != nil {
		return "", err
	}
	path = pwd.getModPathInTemp(tempDir)
	return path, err
}

func (pwd *PackageWithDeps) prepareAndRunInit(pathToModFile string) error {
	log.Debug("Preparing to init", pathToModFile)
	err := os.Chdir(filepath.Dir(pathToModFile))
	if errorutils.CheckError(err) != nil {
		return err
	}
	exists, err := fileutils.IsFileExists(pathToModFile, false)
	logError(err)
	if exists {
		err = os.Remove(pathToModFile)
		logError(err)
	}
	// Mod empty.
	// If empty, run go mod init
	moduleId := pwd.Dependency.GetId()
	moduleInfo := strings.Split(moduleId, ":")
	return cmd.RunGoModInit(replaceExclamationMarkWithUpperCase(moduleInfo[0]), pwd.GoModEditMessage)
}

func writeModContentToModFile(path string, modContent []byte) error {
	return ioutil.WriteFile(path, modContent, 0700)
}

func (pwd *PackageWithDeps) getModPathInTemp(tempDir string) string {
	moduleId := pwd.Dependency.GetId()
	moduleInfo := strings.Split(moduleId, ":")
	moduleInfo[0] = replaceExclamationMarkWithUpperCase(moduleInfo[0])
	moduleId = strings.Join(moduleInfo, ":")
	modulePath := strings.Replace(moduleId, ":", "@", 1)
	path := filepath.Join(tempDir, modulePath, "go.mod")
	return path
}

func (pwd *PackageWithDeps) publishDependencyAndPopulateTransitive(pathToModFile, targetRepo string, graphDependencies map[string]bool, cache *cache.DependenciesCache, serviceManager *artifactory.ArtifactoryServicesManager) error {
	// If the mod is not empty, populate transitive dependencies
	if len(graphDependencies) > 0 {
		sumFileContent , sumFileStat, err := cmd.GetSumContentAndRemove(filepath.Dir(pathToModFile))
		logError(err)
		pwd.setTransitiveDependencies(targetRepo, graphDependencies, cache, serviceManager.GetConfig().GetArtDetails())
		if len(sumFileContent) > 0 && sumFileStat != nil {
			cmd.RestoreSumFile(filepath.Dir(pathToModFile), sumFileContent, sumFileStat)
		}
	}

	published, _ := cache.GetMap()[pwd.Dependency.GetId()]
	if !published && (pwd.PatternMatched(pwd.regExp.GetNotEmptyModRegex()) || pwd.PatternMatched(pwd.regExp.GetGeneratedBy())) {
		err := pwd.writeModContentToGoCache()
		logError(err)
	}

	// Populate and publish the transitive dependencies.
	if pwd.transitiveDependencies != nil {
		pwd.populateTransitive(targetRepo, cache, serviceManager)
	}

	if !published && pwd.shouldRevertToEmptyMod {
		log.Debug("Reverting to the original mod of", pwd.Dependency.GetId())
		editedBy := pwd.regExp.GetGeneratedBy()
		if editedBy.FindString(string(pwd.originalModContent)) == "" {
			pwd.originalModContent = append([]byte(pwd.GoModEditMessage+"\n\n"), pwd.originalModContent...)
		}
		writeModContentToModFile(pathToModFile, pwd.originalModContent)
		pwd.Dependency.SetModContent(pwd.originalModContent)
		err := pwd.writeModContentToGoCache()
		logError(err)
	}
	// Publish to Artifactory the dependency if needed.
	if !published {
		err := pwd.prepareAndPublish(targetRepo, cache, serviceManager)
		logError(err)
	}

	// Remove from temp folder the dependency.
	err := os.RemoveAll(filepath.Dir(pathToModFile))
	if errorutils.CheckError(err) != nil {
		log.Error(fmt.Sprintf("Removing the following directory %s has encountred an error: %s", err, filepath.Dir(pathToModFile)))
	}

	return nil
}

// Prepare for publishing and publish the dependency to Artifactory
// Mark this dependency as published
func (pwd *PackageWithDeps) prepareAndPublish(targetRepo string, cache *cache.DependenciesCache, serviceManager *artifactory.ArtifactoryServicesManager) error {
	err := pwd.Dependency.prepareAndPublish(targetRepo, cache, serviceManager)
	cache.GetMap()[pwd.Dependency.GetId()] = true
	return err
}

func (pwd *PackageWithDeps) setTransitiveDependencies(targetRepo string, graphDependencies map[string]bool, cache *cache.DependenciesCache, auth auth.ArtifactoryDetails) {
	var dependencies []PackageWithDeps
	for transitiveDependency := range graphDependencies {
		module := strings.Split(transitiveDependency, "@")
		if len(module) == 2 {
			dependenciesMap := cache.GetMap()
			name := getDependencyName(module[0])
			_, exists := dependenciesMap[name+":"+module[1]]
			if !exists {
				// Check if the dependency is in the local cache.
				dep, err := createDependency(pwd.cachePath, name, module[1])
				logError(err)
				if err != nil {
					continue
				}
				// Check if this dependency exists in Artifactory.
				client := httpclient.NewDefaultHttpClient()
				downloadedFromArtifactory, err := shouldDownloadFromArtifactory(module[0], module[1], targetRepo, auth, client)
				logError(err)
				if err != nil {
					continue
				}
				if dep == nil {
					// Dependency is missing in the local cache. Need to download it...
					dep, err = downloadAndCreateDependency(pwd.cachePath, name, module[1], transitiveDependency, targetRepo, downloadedFromArtifactory, auth)
					logError(err)
					if err != nil {
						continue
					}
				}

				if dep != nil {
					log.Debug(fmt.Sprintf("Dependency %s has transitive dependency %s", pwd.Dependency.GetId(), dep.GetId()))
					depsWithTrans := &PackageWithDeps{Dependency: dep,
						regExp:           pwd.regExp,
						cachePath:        pwd.cachePath,
						TidyEnum:         pwd.TidyEnum,
						GoModEditMessage: pwd.GoModEditMessage}
					dependencies = append(dependencies, *depsWithTrans)
					dependenciesMap[name+":"+module[1]] = downloadedFromArtifactory
				}
			} else {
				log.Debug("Dependency", transitiveDependency, "has been previously added.")
			}
		}
	}
	pwd.transitiveDependencies = dependencies
}

func (pwd *PackageWithDeps) writeModContentToGoCache() error {
	moduleAndVersion := strings.Split(pwd.Dependency.GetId(), ":")
	pathToModule := strings.Split(moduleAndVersion[0], "/")
	path := filepath.Join(pwd.cachePath, strings.Join(pathToModule, fileutils.GetFileSeparator()), "@v", moduleAndVersion[1]+".mod")
	err := ioutil.WriteFile(path, pwd.Dependency.GetModContent(), 0700)
	return errorutils.CheckError(err)
}

// Runs over the transitive dependencies, populate the mod files of those transitive dependencies
func (pwd *PackageWithDeps) populateTransitive(targetRepo string, cache *cache.DependenciesCache, serviceManager *artifactory.ArtifactoryServicesManager) {
	cache.IncrementTotal(len(pwd.transitiveDependencies))
	for _, transitiveDep := range pwd.transitiveDependencies {
		published, _ := cache.GetMap()[transitiveDep.Dependency.GetId()]
		if !published {
			log.Debug("Starting to work on transitive dependency:", transitiveDep.Dependency.GetId())
			transitiveDep.PopulateModAndPublish(targetRepo, cache, serviceManager)
		} else {
			cache.IncrementSuccess()
			log.Debug("The dependency", transitiveDep.Dependency.GetId(), "was already handled")
		}
	}
}
