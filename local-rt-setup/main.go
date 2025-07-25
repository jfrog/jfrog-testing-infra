package main

import (
	"bufio"
	"bytes"
	"context"
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
	maxConnectionWaitSeconds = 900
	waitSleepIntervalSeconds = 15
	requestTimeout           = 30 * time.Second
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
	tryingLog                   = fmt.Sprintf("Trying again in %d seconds.", waitSleepIntervalSeconds)
	dumpLogBuffer               = make([]byte, 64*1024) // 64KB buffer for log dumping

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
		dumpLogs(jfrogHome)
		return err
	}

	if err = waitForArtifactorySuccessfulPing(); err != nil {
		return err
	}

	if !artifactory6 {
		adminToken, err := generateAccessToken()
		if err != nil {
			dumpLogs(jfrogHome)
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
	return os.WriteFile(artifactoryCommonPath, updatedContent, 0o755)
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
// extractOutputData - if true, the response body will be processed and returned.
func runInRetryLoop(doRequest func(ctx context.Context) (*http.Response, error), successMessage string, extractOutputData bool) (respBody []byte, err error) {
	for timeElapsed := 0; timeElapsed < maxConnectionWaitSeconds; timeElapsed += waitSleepIntervalSeconds {
		respBody, err, stop := tryRequest(doRequest, extractOutputData)
		if stop {
			log.Println(successMessage)
			return respBody, err
		}
		time.Sleep(time.Second * waitSleepIntervalSeconds)
	}
	err = errors.Join(err, fmt.Errorf("could not connect to Artifactory, reached timeout of %d seconds", maxConnectionWaitSeconds))
	return
}

func tryRequest(doRequest func(ctx context.Context) (*http.Response, error), extractOutputData bool) (respData []byte, err error, stop bool) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	var response *http.Response
	if response, err = doRequest(ctx); err != nil {
		log.Printf("Received error: %s. %s", err, tryingLog)
	} else {
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			respData, err = processResponseOutput(response, extractOutputData)
			stop = true
		} else {
			_, _ = processResponseOutput(response, false) // Discard the response body
		}
		if !stop {
			log.Printf("Artifactory response: %d. %s", response.StatusCode, tryingLog)
		}
	}

	return
}

func processResponseOutput(resp *http.Response, processOutput bool) (output []byte, err error) {
	if resp == nil {
		return nil, errors.New("response is nil")
	}

	defer closeQuietly(resp.Body, "error when closing response body after reading")

	if processOutput {
		output, err = io.ReadAll(resp.Body)
		if err != nil {
			err = fmt.Errorf("error reading response body: %w", err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return
}

func waitForArtifactorySuccessfulPing() (err error) {
	log.Println("Waiting for successful connection with Artifactory...")
	_, err = runInRetryLoop(ping, "Artifactory is up!", false)
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
	return os.WriteFile(filepath.Join(jfrogHome, artifactoryVarEtcPath, "system.yaml"), []byte(systemYaml), 0o611)
}

// Create access.config.import.yml file in the etc/access directory.
func createAccessConfig(jfrogHome string) error {
	return os.WriteFile(filepath.Join(jfrogHome, artifactoryVarEtcAccessPath, "access.config.import.yml"), []byte(accessConfig), 0o611)
}

// Allow using staging mode in Artifactory.
func allowStagingMode(jfrogHome string) error {
	systemPropertiesPath := filepath.Join(jfrogHome, artifactoryVarEtcPath, "artifactory", "artifactory.system.properties")
	return os.WriteFile(systemPropertiesPath, []byte("staging.mode=true\n"), 0o611)
}

// More info at: https://docs.github.com/en/github-ae@latest/actions/using-workflows/workflow-commands-for-github-actions#environment-files
func exportTokenUsingGithubEnvFile(adminToken string) (err error) {
	githubEnvPath, exists := os.LookupEnv(githubEnvFileEnv)
	if !exists {
		log.Printf("GITHUB_ENV not set, assuming the script is not running on Github. Skipping token export...")
		return
	}

	githubEnvFile, err := os.OpenFile(githubEnvPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return
	}

	defer closeQuietly(githubEnvFile, "error when closing github env file")

	if _, err = githubEnvFile.WriteString(fmt.Sprintf("%s=%s\n", jfrogLocalAccessToken, adminToken)); err != nil {
		return
	}
	log.Printf("Successfuly exported Artifactory admin token to github_env...")
	return
}

func generateAccessToken() (accessToken string, err error) {
	log.Println("Generating access token...")
	var respBody []byte
	if respBody, err = runInRetryLoop(doGenerateAccessToken, "Successfully generated an access token!", true); err != nil {
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

func doGenerateAccessToken(ctx context.Context) (*http.Response, error) {
	requestContent, err := json.Marshal(tokenInfo{Audience: "*@*"})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokensApi, bytes.NewBuffer(requestContent))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	req.Header.Set("Content-Type", "application/json")

	return http.DefaultClient.Do(req)
}

func ping(ctx context.Context) (*http.Response, error) {
	url := localArtifactoryUrl + "api/system/ping"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
	closeQuietly(resp.Body, "error when closing body after setting custom url base")

	// Artifactory might return 500 because the url has allegedly changed.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		return fmt.Errorf("failed setting custom url. response: %d", resp.StatusCode)
	}

	// Verify connection after setting custom url.
	pingCtx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if resp, err = ping(pingCtx); err != nil {
		return err
	}

	closeQuietly(resp.Body, "error when closing body after ping")

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
	defer closeQuietly(resp.Body, "error when closing body after download")

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
	defer closeQuietly(file, "error when closing archive file")
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
	return os.WriteFile(fileDest, []byte(license), 0o500)
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

	// <archiveIndexEnabled> property is removed from default application configuration since 7.111.4
	if strings.Contains(confStr, getArchiveIndexEnabledAttribute(false)) {
		log.Println("Found archive index enabled attribute, updating to true")
		confStr = strings.ReplaceAll(confStr, getArchiveIndexEnabledAttribute(false), getArchiveIndexEnabledAttribute(true))
	}

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

	defer closeQuietly(resp.Body, "error when closing body after download")

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

func closeQuietly(closer io.Closer, message string) {
	if err := closer.Close(); err != nil {
		log.Println(message, err)
	}
}

func dumpLogFile(jfrogHome, fileName string) {
	log.Printf("\n\n=========== %s ===========\n", fileName)
	logFilePath := filepath.Join(jfrogHome, artifactoryVarPath, "log", fileName)
	if _, err := os.Stat(logFilePath); os.IsNotExist(err) {
		log.Printf("Log file %s does not exist. Skipping dump.", logFilePath)
		return
	}

	logFile, err := os.OpenFile(logFilePath, os.O_RDONLY, 0o644)
	if err != nil {
		log.Printf("Error opening log file %s: %v", logFilePath, err)
		return
	}

	defer closeQuietly(logFile, "error when closing log file")

	out := bufio.NewWriter(os.Stdout)
	defer func() {
		_ = out.Flush()
	}()

	_, err = io.CopyBuffer(out, logFile, dumpLogBuffer)
	if err != nil {
		log.Printf("Error reading log file %s: %v", logFilePath, err)
		return
	}
}

func dumpLogs(jfrogHome string) {
	dumpLogFile(jfrogHome, "artifactory-service.log")
	dumpLogFile(jfrogHome, "access-service.log")
	dumpLogFile(jfrogHome, "router-service.log")
}

type tokenInfo struct {
	AccessToken string `json:"access_token,omitempty"`
	Audience    string `json:"audience,omitempty"`
}
