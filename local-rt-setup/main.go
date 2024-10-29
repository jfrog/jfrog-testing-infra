package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jfrog/archiver/v3"
)

const (
	maxConnectionWaitSeconds = 300
	waitSleepIntervalSeconds = 10
	jfrogHomeEnv             = "JFROG_HOME"
	licenseEnv               = "RTLIC"
	localArtifactoryUrl      = "http://localhost:8081/artifactory/"
	// #nosec G101 -- False positive - no hardcoded credentials.
	tokensApi         = "http://localhost:8082/access/api/v1/tokens"
	defaultUsername   = "admin"
	defaultPassword   = "password"
	defaultVersion    = "[RELEASE]"
	tokenJson         = "token.json"
	generateTokenJson = "generate.token.json"
	githubEnvFileEnv  = "GITHUB_ENV"
	// #nosec G101 -- False positive - no hardcoded credentials
	jfrogLocalAccessToken = "JFROG_TESTS_LOCAL_ACCESS_TOKEN"
)

var (
	artifactoryVarPath          = filepath.Join("artifactory", "var")
	artifactoryVarEtcPath       = filepath.Join(artifactoryVarPath, "etc")
	artifactoryVarEtcAccessPath = filepath.Join(artifactoryVarEtcPath, "access")
	artifactoryAppBinPath       = filepath.Join("artifactory", "app", "bin")

	//go:embed system.yaml
	systemYaml string
	//go:embed access.config.import.yml
	accessConfig string
)

func main() {
	if err := setupLocalArtifactory(); err != nil {
		log.Fatal(err)
	}
}

func setupLocalArtifactory() (err error) {
	license := os.Getenv(licenseEnv)
	if license == "" {
		return errors.New("no license provided. Aborting. Provide license by setting the '" + licenseEnv + "' env var")
	}

	jfrogHome, err := prepareJFrogHome()
	if err != nil {
		return err
	}

	rtVersion := flag.String("rt-version", defaultVersion, "the version of Artifactory to download")
	flag.Parse()
	artifactory6 := false
	if *rtVersion != defaultVersion {
		versionParts := strings.Split(*rtVersion, ".")
		if len(versionParts) != 3 {
			return errors.New("the Artifactory version is invalid. It must be [RELEASE] or match this format: X.X.X")
		}
		majorVer, err := strconv.Atoi(versionParts[0])
		if err != nil {
			return err
		}
		if majorVer < 6 {
			return errors.New("this tool supports Artifactory 6 or higher")
		}
		artifactory6 = majorVer == 6
	}

	pathToArchive, err := downloadArtifactory(jfrogHome, *rtVersion, artifactory6)
	if err != nil {
		return err
	}

	if err = extract(pathToArchive, jfrogHome); err != nil {
		return err
	}

	if err = renameArtifactoryDir(jfrogHome); err != nil {
		return err
	}

	if !artifactory6 && isMac() {
		if err = os.Chmod(filepath.Join(jfrogHome, artifactoryVarPath), os.ModePerm); err != nil {
			return err
		}
		if err = fixBash3Compatibility(jfrogHome); err != nil {
			return err
		}
	}

	if err = createLicenseFile(jfrogHome, license, artifactory6); err != nil {
		return err
	}

	var binDir string
	if artifactory6 {
		binDir = filepath.Join(jfrogHome, "artifactory", "bin")
	} else {
		binDir = filepath.Join(jfrogHome, "artifactory", "app", "bin")
		if err = handleArtifactory7(jfrogHome); err != nil {
			return err
		}
	}

	if err = startArtifactory(binDir); err != nil {
		return err
	}

	if err = waitForArtifactorySuccessfulPing(); err != nil {
		return err
	}

	if !artifactory6 {
		adminToken, err := generateAccessToken()
		if err != nil {
			return err
		}
		if err = exportTokenUsingGithubEnvFile(adminToken); err != nil {
			return err
		}
	}

	if err = setCustomUrlBase(); err != nil || artifactory6 {
		return err
	}

	return enableArchiveIndex()
}

// Fix the bash 3 compatibility issue by removing the ,, from the artifactoryCommon.sh file.
func fixBash3Compatibility(jfrogHome string) error {
	artifactoryCommonPath := filepath.Join(jfrogHome, artifactoryAppBinPath, "artifactoryCommon.sh")

	// Read artifactoryCommon.sh file
	content, err := os.ReadFile(artifactoryCommonPath)
	if err != nil {
		return err
	}

	// Replace ,, with an empty string
	updatedContent := bytes.ReplaceAll(content, []byte(",,"), []byte{})

	// Write artifactoryCommon.sh without the ,,
	return os.WriteFile(artifactoryCommonPath, updatedContent, 0755)
}

// Rename the directory that was extracted from the archive, to easily access in the rest of the script.
func renameArtifactoryDir(jfrogHome string) error {
	fileInfo, err := os.ReadDir(jfrogHome)
	if err != nil {
		return err
	}

	for _, file := range fileInfo {
		if file.IsDir() && strings.HasPrefix(file.Name(), "artifactory-pro-") {
			return os.Rename(filepath.Join(jfrogHome, file.Name()), filepath.Join(jfrogHome, "artifactory"))
		}
	}
	return errors.New("artifactory dir was not found after extracting")
}

// Creates and sets the jfrog home directory at the user's home directory, or as provided by the JFROG_HOME environment variable.
func prepareJFrogHome() (string, error) {
	// Read JFROG_HOME environment variable
	jfrogHome := os.Getenv(jfrogHomeEnv)

	// If JFROG_HOME environment variable is not set, set JFROG_HOME=${USER_HOME}/jfrog_home
	if jfrogHome == "" {
		wd, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}

		jfrogHome = filepath.Join(wd, "jfrog_home")
		if err = os.Setenv(jfrogHomeEnv, jfrogHome); err != nil {
			return "", err
		}
	}

	// Create jfrog_home directory if needed
	exists, err := isExists(jfrogHome)
	if err != nil {
		return "", err
	}
	if !exists {
		return jfrogHome, os.MkdirAll(jfrogHome, os.ModePerm)
	}

	// If jfrog_home/artifactory directory already exists, return error
	if exists, err = isExists(filepath.Join(jfrogHome, "artifactory")); err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("artifactory dir already exists in jfrog home: " + filepath.Join(jfrogHome, "artifactory"))
	}
	return jfrogHome, nil
}

func startArtifactory(binDir string) error {
	log.Println("Starting Artifactory...")
	var cmd *exec.Cmd
	if isWindows() {
		cmd = exec.Command(filepath.Join(binDir, "InstallService.bat"))
	} else {
		cmd = exec.Command(filepath.Join(binDir, "artifactoryctl"), "start")
	}
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	return cmd.Run()
}

// The function will retry connecting to Artifactory every 10 seconds, for a maximum of 300 seconds.
// If the connection is successful, the function will return the response body.
// doRequest - the function to run in the retry loop.
// successMessage - the message to print when the connection is successful.
func runInRetryLoop(doRequest func() (*http.Response, error), successMessage string) (respBody []byte, err error) {
	tryingLog := fmt.Sprintf("Trying again in %d seconds.", waitSleepIntervalSeconds)
	for timeElapsed := 0; timeElapsed < maxConnectionWaitSeconds; timeElapsed += waitSleepIntervalSeconds {
		time.Sleep(time.Second * waitSleepIntervalSeconds)

		var response *http.Response
		if response, err = doRequest(); err != nil {
			log.Printf("Receieved error: %s. %s", err, tryingLog)
		} else {
			respBody, err = io.ReadAll(response.Body)
			if err != nil {
				return
			}
			defer func() {
				err = errors.Join(err, response.Body.Close())
			}()
			if response.StatusCode == http.StatusOK {
				log.Println(successMessage)
				return
			} else {
				log.Printf("Artifactory response: %d. %s", response.StatusCode, tryingLog)
			}
		}
	}
	err = errors.New("could not connect to Artifactory: " + err.Error())
	return
}

func waitForArtifactorySuccessfulPing() (err error) {
	log.Println("Waiting for successful connection with Artifactory...")
	_, err = runInRetryLoop(ping, "Artifactory is up!")
	return
}

func handleArtifactory7(jfrogHome string) error {
	if err := createSystemYaml(jfrogHome); err != nil {
		return err
	}
	if err := allowStagingMode(jfrogHome); err != nil {
		return err
	}
	return createAccessConfig(jfrogHome)
}

// Create system.yaml file in the etc directory.
func createSystemYaml(jfrogHome string) error {
	return os.WriteFile(filepath.Join(jfrogHome, artifactoryVarEtcPath, "system.yaml"), []byte(systemYaml), 0611)
}

// Create access.config.import.yml file in the etc/access directory.
func createAccessConfig(jfrogHome string) error {
	return os.WriteFile(filepath.Join(jfrogHome, artifactoryVarEtcAccessPath, "access.config.import.yml"), []byte(accessConfig), 0611)
}

// Allow using staging mode in Artifactory.
func allowStagingMode(jfrogHome string) error {
	systemPropertiesPath := filepath.Join(jfrogHome, artifactoryVarEtcPath, "artifactory", "artifactory.system.properties")
	return os.WriteFile(systemPropertiesPath, []byte("staging.mode=true\n"), 0611)
}

// More info at: https://docs.github.com/en/github-ae@latest/actions/using-workflows/workflow-commands-for-github-actions#environment-files
func exportTokenUsingGithubEnvFile(adminToken string) (err error) {
	githubEnvPath, exists := os.LookupEnv(githubEnvFileEnv)
	if !exists {
		log.Printf("GITHUB_ENV not set, assuming the script is not running on Github. Skipping token export...")
		return
	}

	githubEnvFile, err := os.OpenFile(githubEnvPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return
	}

	defer func() {
		err = errors.Join(err, githubEnvFile.Close())
	}()

	if _, err = githubEnvFile.WriteString(fmt.Sprintf("%s=%s\n", jfrogLocalAccessToken, adminToken)); err != nil {
		return
	}
	log.Printf("Successfuly exported Artifactory admin token to github_env...")
	return
}

func generateAccessToken() (accessToken string, err error) {
	log.Println("Generating access token...")
	var respBody []byte
	if respBody, err = runInRetryLoop(doGenerateAccessToken, "Successfully generated an access token!"); err != nil {
		return "", err
	}

	var tokenParams tokenInfo
	if err = json.Unmarshal(respBody, &tokenParams); err != nil {
		return "", err
	}
	if tokenParams.AccessToken == "" {
		return "", errors.New("admin Access Token is empty")
	}
	return tokenParams.AccessToken, nil
}

func doGenerateAccessToken() (*http.Response, error) {
	requestContent, err := json.Marshal(tokenInfo{Audience: "*@*"})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, tokensApi, bytes.NewBuffer(requestContent))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	req.Header.Set("Content-Type", "application/json")

	return http.DefaultClient.Do(req)
}

func ping() (*http.Response, error) {
	url := localArtifactoryUrl + "api/system/ping"

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	return http.DefaultClient.Do(req)
}

// Custom URL base is required when creating federated repositories.
func setCustomUrlBase() error {
	log.Println("Setting custom URL base...")

	url := localArtifactoryUrl + "api/system/configuration/baseUrl"
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer([]byte(localArtifactoryUrl)))
	if err != nil {
		return err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if err = resp.Body.Close(); err != nil {
		return err
	}

	// Artifactory might return 500 because the url has allegedly changed.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		return fmt.Errorf("failed setting custom url. response: %d", resp.StatusCode)
	}

	// Verify connection after setting custom url.
	if resp, err = ping(); err != nil {
		return err
	}
	if err = resp.Body.Close(); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed reaching to Artifactory after setting custom url base. response: %d", resp.StatusCode)
	}

	log.Println("Done setting custom URL base.")
	return nil
}

func downloadArtifactory(downloadDest, rtVersion string, artifactory6 bool) (pathToArchive string, err error) {
	url := fmt.Sprintf("https://releases.jfrog.io/artifactory/artifactory-pro/org/artifactory/pro/jfrog-artifactory-pro/%[1]s/jfrog-artifactory-pro-%[1]s", rtVersion)
	if !artifactory6 {
		switch runtime.GOOS {
		case "darwin":
			url += "-darwin.tar.gz"
		case "windows":
			url += "-windows.zip"
		case "linux":
			url += "-linux.tar.gz"
		default:
			return "", errors.New("the OS on this machine is currently unsupported. Supported OS are darwin, windows and linux")
		}
	} else {
		url += ".zip"
	}

	log.Println("Downloading Artifactory from URL: " + url)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed creating new request: %s", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed getting archive: %s", err)
	}
	defer func() {
		if e := resp.Body.Close(); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when closing body after download: " + e.Error())
			}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New("failed downloading Artifactory. Releases response: " + resp.Status)
	}

	// Extract archive file name.
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		return "", err
	}
	filename := params["filename"]
	log.Println("Extracted archive name from response: " + filename)

	pathToArchive = filepath.Join(downloadDest, filename)
	file, err := os.Create(pathToArchive)
	if err != nil {
		return "", err
	}
	defer func() {
		if e := file.Close(); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when closing archive file: " + e.Error())
			}
		}
	}()
	_, err = io.Copy(file, resp.Body)
	return pathToArchive, err
}

func extract(archivePath string, destDir string) error {
	log.Println("Extracting archive...")
	return archiver.Unarchive(archivePath, destDir)
}

func createLicenseFile(jfrogHome, license string, artifactory6 bool) (err error) {
	log.Println("Creating license...")

	defer func() {
		if e := os.Unsetenv(licenseEnv); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when unsetting env: " + e.Error())
			}
		}
	}()

	var fileDest string
	if artifactory6 {
		fileDest = filepath.Join(jfrogHome, "artifactory", "etc", "artifactory.lic")
	} else {
		fileDest = filepath.Join(jfrogHome, artifactoryVarEtcPath, "artifactory", "artifactory.cluster.license")
	}
	return os.WriteFile(fileDest, []byte(license), 0500)
}

func isExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func isMac() bool {
	return runtime.GOOS == "darwin"
}

func isWindows() bool {
	return runtime.GOOS == "windows"
}

func enableArchiveIndex() error {
	log.Println("Enabling archive index...")
	confStr, err := handleConfiguration(http.MethodGet, nil)
	if err != nil {
		return err
	}

	if !strings.Contains(confStr, getArchiveIndexEnabledAttribute(false)) {
		return errors.New("failed setting the archive index property - attribute does not exist in configuration")
	}
	confStr = strings.ReplaceAll(confStr, getArchiveIndexEnabledAttribute(false), getArchiveIndexEnabledAttribute(true))

	// Post new configuration
	_, err = handleConfiguration(http.MethodPost, strings.NewReader(confStr))
	return err
}

func handleConfiguration(method string, body io.Reader) (string, error) {
	url := localArtifactoryUrl + "api/system/configuration"

	log.Println(method + "ing Artifactory configuration...")
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if e := resp.Body.Close(); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when closing body after download: " + e.Error())
			}
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed %sing Artifactory configuration. response: %d", method, resp.StatusCode)
	}

	buf := new(strings.Builder)
	n, err := io.Copy(buf, resp.Body)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", errors.New("failed reading response body")
	}
	return buf.String(), nil
}

func getArchiveIndexEnabledAttribute(value bool) string {
	return fmt.Sprintf("<archiveIndexEnabled>%v</archiveIndexEnabled>", value)
}

type tokenInfo struct {
	AccessToken string `json:"access_token,omitempty"`
	Audience    string `json:"audience,omitempty"`
}
