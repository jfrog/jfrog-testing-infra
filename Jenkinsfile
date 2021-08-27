/**
* Track a test suite for a GitHub repository. Set the job results according to the README badge.
* Input - BADGE_LINK - Link to the README badge, for example:
*   https://badgen.net/github/status/jfrog/jfrog-cli/v2?label=JFrog%20Pipelines.
* Output - Set this job status SUCCESS, FAILURE, or NOT_BUILT.
*/
properties([
        buildDiscarder(logRotator(numToKeepStr: '5'))
])
timeout(time: 45, unit: 'SECONDS') {
    node {
        response = httpRequest(consoleLogResponseBody: true,
                contentType: 'APPLICATION_JSON',
                httpMode: 'GET',
                url: BADGE_LINK,
                validResponseCodes: '200')

        if (response.content.contains('failing')) {
            currentBuild.result = 'FAILURE'
        } else if (response.content.contains('passing')) {
            currentBuild.result = 'SUCCESS'
        } else {
            currentBuild.result = 'NOT_BUILT'
        }
    }
}
