package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/mholt/archiver/v3"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxConnectionWaitSeconds = 300
const waitSleepIntervalSeconds = 10
const jfrogHomeEnv = "JFROG_HOME"
const licenseEnv = "RTLIC"
const localArtifactoryUrl = "http://localhost:8081/artifactory/"

func main() {
	err := setupLocalArtifactory()
	if err != nil {
		log.Fatal(err)
	}
}

func setupLocalArtifactory() (err error) {
	jfrogHome := os.Getenv(jfrogHomeEnv)
	if jfrogHome == "" {
		jfrogHome, err = setJfrogHome()
		if err != nil {
			return err
		}
	}

	exists, err := isExists(filepath.Join(jfrogHome, "artifactory"))
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("artifactory dir already exists in jfrog home: " + filepath.Join(jfrogHome, "artifactory"))
	}

	pathToArchive, err := downloadArtifactory(jfrogHome)
	if err != nil {
		return err
	}

	err = extract(pathToArchive, jfrogHome)
	if err != nil {
		return err
	}

	err = renameArtifactoryDir(jfrogHome)
	if err != nil {
		return err
	}

	if isMac() {
		err = os.Chmod(filepath.Join(jfrogHome, "artifactory", "var"), os.ModePerm)
		if err != nil {
			return err
		}
	}

	err = createLicenseFile(jfrogHome)
	if err != nil {
		return err
	}

	err = startArtifactory(jfrogHome)
	if err != nil {
		return err
	}

	err = waitForArtifactorySuccessfulPing()
	if err != nil {
		return err
	}

	return setCustomUrlBase()
}

// Rename the directory that was extracted from the archive, to easily access in the rest of the script.
func renameArtifactoryDir(jfrogHome string) error {
	fileInfo, err := ioutil.ReadDir(jfrogHome)
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

// Creates and sets the jfrog home directory at the parent of the working directory.
func setJfrogHome() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	jfrogHome := filepath.Join(filepath.Dir(wd), "jfrog_home")
	err = os.MkdirAll(jfrogHome, os.ModePerm)
	if err != nil {
		return "", err
	}

	err = os.Setenv(jfrogHomeEnv, jfrogHome)
	if err != nil {
		return "", err
	}
	return jfrogHome, err
}

func startArtifactory(jfrogHome string) error {
	log.Println("Starting Artifactory...")

	binDir := filepath.Join(jfrogHome, "artifactory", "app", "bin")

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

func waitForArtifactorySuccessfulPing() error {
	log.Println("Waiting for successful connection with Artifactory...")
	tryingLog := fmt.Sprintf("Trying again in %d seconds.", waitSleepIntervalSeconds)

	for timeElapsed := 0; timeElapsed < maxConnectionWaitSeconds; timeElapsed += waitSleepIntervalSeconds {
		time.Sleep(time.Second * waitSleepIntervalSeconds)

		response, err := ping()
		if err != nil {
			log.Printf("Receieved error: %s. %s", err, tryingLog)
		} else {
			err = response.Body.Close()
			if err != nil {
				return err
			}
			if response.StatusCode == http.StatusOK {
				log.Println("Artifactory is up!")
				return nil
			} else {
				log.Printf("Artifactory response: %d. %s", response.StatusCode, tryingLog)
			}
		}
	}
	return errors.New("could not connect to Artifactory")
}

func ping() (*http.Response, error) {
	url := localArtifactoryUrl + "api/system/ping"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// Custom URL base is required when creating federated repositories.
func setCustomUrlBase() error {
	log.Println("Setting custom URL base...")

	url := localArtifactoryUrl + "api/system/configuration/baseUrl"
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer([]byte(localArtifactoryUrl)))
	if err != nil {
		return err
	}
	req.SetBasicAuth("admin", "password")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	err = resp.Body.Close()
	if err != nil {
		return err
	}

	// Artifactory might return 500 because the url has allegedly changed.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		return fmt.Errorf("failed setting custom url. response: %d", resp.StatusCode)
	}

	// Verify connection after setting custom url.
	resp, err = ping()
	if err != nil {
		return err
	}
	err = resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed reaching to Artifactory after setting custom url base. response: %d", resp.StatusCode)
	}

	log.Println("Done setting custom URL base.")
	return nil
}

func downloadArtifactory(downloadDest string) (pathToArchive string, err error) {
	url := "https://releases.jfrog.io/artifactory/artifactory-pro/org/artifactory/pro/jfrog-artifactory-pro/[RELEASE]/jfrog-artifactory-pro-[RELEASE]-"
	switch runtime.GOOS {
	case "darwin":
		url += "darwin.tar.gz"
	case "windows":
		url += "windows.zip"
	case "linux":
		url += "linux.tar.gz"
	default:
		return "", errors.New("the OS on this machine is currently unsupported. Supported OS are darwin, windows and linux")
	}

	log.Println("Downloading Artifactory from URL: " + url)

	req, err := http.NewRequest("GET", url, nil)
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

func createLicenseFile(jfrogHome string) (err error) {
	log.Println("Creating license...")

	license := os.Getenv(licenseEnv)
	if license == "" {
		log.Println("No license provided. Skipping.")
	}
	defer func() {
		if e := os.Unsetenv(licenseEnv); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when unsetting env: " + e.Error())
			}
		}
	}()

	fileDest := filepath.Join(jfrogHome, "artifactory", "var", "etc", "artifactory", "artifactory.cluster.license")
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
