# JFrog Testing Infra

This project includes:
1. The common testing code used by integration tests of [Jenkins Artifactory plugin](https://github.com/jfrog/jenkins-artifactory-plugin) and the [Bamboo Artifactory plugin](https://github.com/jfrog/bamboo-artifactory-plugin). Located under the java subdirectory.
2. Go script to set up a local Artifactory, by downloading the Artifactory archive corresponding to the operating system on the machine. Located under the local-rt-setup subdirectory.
3. A Jenkinsfile, which used to monitor a GitHub badge. This Jenkinsfile is used for populating a tests dashboard in Jenkins. Read more about how this Jenkinsfile is used in the description inside the Jenkinsfile. Located under the root directory.

## Building the java sources

To build the library sources, please follow these steps:

1. Clone the code from Git.
2. Install the library by running the following commands:

```
cd java
./gradlew clean compileJava publishToMavenLocal
```

## Code Contributions

We welcome community contribution through pull requests.
