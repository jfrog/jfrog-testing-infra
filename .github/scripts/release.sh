#!/usr/bin/env bash
set -euo pipefail

# Release script for the jfrog-testing-infra Java/Gradle project.
# Bumps the release version, runs the JFrog CLI Gradle build, publishes build info,
# distributes the release bundle, then bumps to the next development version.
# Used by .github/workflows/release.yml, but can also be run directly on a
# developer machine from anywhere in the repo checkout.
#
# Expected environment variables:
#   NEXT_VERSION               - version to release (e.g. 1.2.3)
#   NEXT_DEVELOPMENT_VERSION   - next development/snapshot version (e.g. 1.2.4-SNAPSHOT)
#   ARTIFACTORY_URL            - Artifactory base URL
#   ARTIFACTORY_USER           - Artifactory username
#   ARTIFACTORY_APIKEY         - Artifactory API key/password
#   JFROG_CLI_BUILD_NAME       - build name used by jf rt/ds commands (defaults set in CI)
#   JFROG_CLI_BUILD_NUMBER     - build number used by jf rt/ds commands (defaults set in CI)
#   JFROG_CLI_BUILD_PROJECT    - JFrog project key used by jf rt/ds commands (defaults set in CI)
#   JFROG_BUILD_STATUS         - build status recorded in build info (defaults set in CI)
#
# Requires: `jf` (JFrog CLI) and `git` on PATH, with push access to the repository
# and the working tree checked out on the branch to release from.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO_ROOT/java"

# Configure git identity
git config user.name "JFrog CI"
git config user.email "eco-system@jfrog.com"

# Check required inputs
test -n "$NEXT_VERSION" -a "$NEXT_VERSION" != "0.0.0"
test -n "$NEXT_DEVELOPMENT_VERSION" -a "$NEXT_DEVELOPMENT_VERSION" != "0.0.0"

# Configure JFrog CLI servers
jf c rm --quiet
jf c add internal --url=$ARTIFACTORY_URL --user=$ARTIFACTORY_USER --password=$ARTIFACTORY_APIKEY
jf gradlec --use-wrapper --repo-resolve ecosys-maven-remote --repo-deploy ecosys-oss-release-local

# Run audit
jf audit --gradle

# Update release version
sed -i -e "/version=/ s/=.*/=$NEXT_VERSION/" gradle.properties
git commit -am "[artifactory-release] Release version ${NEXT_VERSION} [skipRun]" --allow-empty
git tag ${NEXT_VERSION}

# Push release commit and tag
git push
git push --tags

# Build and publish to Artifactory
jf gradle clean aP

# Publish build info
jf rt bag && jf rt bce
jf rt bp

# Distribute release bundle
jf ds rbc ecosystem-testing-infra $NEXT_VERSION --spec=../release/specs/prod-rbc-filespec.json --spec-vars="version=$NEXT_VERSION" --sign
jf ds rbd ecosystem-testing-infra $NEXT_VERSION --site="releases.jfrog.io" --sync

# Update next development version
sed -i -e "/version=/ s/=.*/=$NEXT_DEVELOPMENT_VERSION/" gradle.properties
git commit -am "[artifactory-release] Next development version [skipRun]"

# Push changes
git push
