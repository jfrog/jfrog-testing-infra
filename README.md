# JFrog Testing Infra

This project includes the common testing code used by integration tests of [Jenkins Artifactory plugin](https://github.com/jfrog/jenkins-artifactory-plugin) and the [Bamboo Artifactory plugin](https://github.com/jfrog/bamboo-artifactory-plugin).
This project also includes a Jenkinsfile, which used to monitor a GitHub badge. This Jenkinsfile is used for populating a tests dashboard in Jenkins. Read more about how this Jenkinsfile is used in the description inside the Jenkinsfile.

## Building the sources

To build the library sources, please follow these steps:

1. Clone the code from Git.
2. Install the library by running the following Gradle command:

```
./gradlew clean publishToMavenLocal
```

## Code Contributions

We welcome community contribution through pull requests.
