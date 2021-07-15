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

        if (response.content.contains("failing")) {
            currentBuild.result = 'FAILURE'
        } else if (response.content.contains("passing")) {
            currentBuild.result = 'SUCCESS'
        } else {
            currentBuild.result = 'NOT_BUILT'
        }
    }
}