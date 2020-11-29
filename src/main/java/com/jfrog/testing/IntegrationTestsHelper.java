package com.jfrog.testing;

import org.apache.commons.collections4.CollectionUtils;
import org.apache.commons.io.IOUtils;
import org.apache.commons.lang.ArrayUtils;
import org.apache.commons.lang.text.StrSubstitutor;
import org.apache.commons.lang3.StringUtils;
import org.apache.commons.lang3.exception.ExceptionUtils;
import org.jfrog.artifactory.client.Artifactory;
import org.jfrog.artifactory.client.ArtifactoryClientBuilder;
import org.jfrog.artifactory.client.ArtifactoryRequest;
import org.jfrog.artifactory.client.RepositoryHandle;
import org.jfrog.artifactory.client.impl.ArtifactoryRequestImpl;
import org.jfrog.artifactory.client.model.LightweightRepository;
import org.jfrog.artifactory.client.model.RepoPath;
import org.jfrog.artifactory.client.model.RepositoryType;
import org.jfrog.build.api.Artifact;
import org.jfrog.build.api.Build;
import org.jfrog.build.api.Dependency;
import org.jfrog.build.api.Module;
import org.jfrog.build.api.util.NullLog;
import org.jfrog.build.extractor.clientConfiguration.client.ArtifactoryBuildInfoClient;

import java.io.IOException;
import java.io.InputStream;
import java.io.UnsupportedEncodingException;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.Arrays;
import java.util.List;
import java.util.Properties;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import java.util.stream.Collectors;

import static org.jfrog.artifactory.client.model.impl.RepositoryTypeImpl.*;
import static org.junit.Assert.*;

/**
 * @author yahavi
 */
@SuppressWarnings("unused")
public class IntegrationTestsHelper implements AutoCloseable {

    // Environment variables names
    public static final String ARTIFACTORY_USERNAME_ENV = "ARTIFACTORY_USERNAME";
    public static final String ARTIFACTORY_PASSWORD_ENV = "ARTIFACTORY_PASSWORD";
    public static final String ARTIFACTORY_URL_ENV = "ARTIFACTORY_URL";

    // Environment variables values
    public static final String ARTIFACTORY_USERNAME = System.getenv(ARTIFACTORY_USERNAME_ENV);
    public static final String ARTIFACTORY_PASSWORD = System.getenv(ARTIFACTORY_PASSWORD_ENV);
    public static final String ARTIFACTORY_URL = System.getenv(ARTIFACTORY_URL_ENV);

    // The repository timestamp. Used to provide uniqueness across parallel test runs.
    public static long repoTimestamp = System.currentTimeMillis();

    private static final Pattern REPO_PATTERN = Pattern.compile("^jfrog-rt-tests(-\\w*)+-(\\d*)$");

    private final ArtifactoryBuildInfoClient buildInfoClient;
    private final Artifactory artifactoryClient;

    public IntegrationTestsHelper() {
        verifyEnvironment();
        buildInfoClient = new ArtifactoryBuildInfoClient(ARTIFACTORY_URL, ARTIFACTORY_USERNAME, ARTIFACTORY_PASSWORD, new NullLog());
        artifactoryClient = ArtifactoryClientBuilder.create()
                .setUrl(ARTIFACTORY_URL)
                .setUsername(ARTIFACTORY_USERNAME)
                .setPassword(ARTIFACTORY_PASSWORD)
                .build();
    }

    /**
     * Verify Artifactory environment variables for the tests.
     */
    private void verifyEnvironment() {
        verifyEnvironment(ARTIFACTORY_URL_ENV);
        verifyEnvironment(ARTIFACTORY_USERNAME_ENV);
        verifyEnvironment(ARTIFACTORY_PASSWORD_ENV);
    }

    /**
     * Verify a single environment variable.
     *
     * @param envKey - The environment variable key to verify
     */
    public static void verifyEnvironment(String envKey) {
        if (StringUtils.isBlank(System.getenv(envKey))) {
            String msg = "'" + envKey + "' environment variable is missing!";
            System.err.println(msg);
            throw new IllegalArgumentException(msg);
        }
    }

    /**
     * Escape backslashes in filesystem path.
     *
     * @param path - Filesystem path to fix
     * @return path compatible with Windows
     */
    public String fixWindowsPath(String path) {
        return StringUtils.replace(path, "\\", "\\\\");
    }

    /**
     * Get the repository key of the temporary test repository.
     *
     * @param repository - The repository base name
     * @return repository key of the temporary test repository
     */
    public static String getRepoKey(TestRepository repository) {
        return String.format("%s-%d", repository.getRepoName(), repoTimestamp);
    }

    /**
     * Clean up old test repositories.
     */
    public void cleanUpArtifactory() {
        Arrays.asList(LOCAL, REMOTE, VIRTUAL).forEach(this::cleanUpRepositoryType);
    }

    /**
     * Clean up old tests repositories with the specified type - Local, Remote or Virtual
     *
     * @param repositoryType - The repository type to delete
     */
    private void cleanUpRepositoryType(RepositoryType repositoryType) {
        artifactoryClient.repositories().list(repositoryType).stream()
                // Get repository key
                .map(LightweightRepository::getKey)

                // Match repository
                .map(REPO_PATTERN::matcher)
                .filter(Matcher::matches)

                // Filter repositories newer than 2 hours
                .filter(this::isRepositoryOld)

                // Get repository key
                .map(Matcher::group)

                // Create repository handle
                .map(artifactoryClient::repository)

                // Delete repository
                .forEach(RepositoryHandle::delete);
    }

    /**
     * Return true if the repository was created more than 2 hours ago.
     *
     * @param repoMatcher - Repo regex matcher on REPO_PATTERN
     * @return true if the repository was created more than 2 hours ago
     */
    private boolean isRepositoryOld(Matcher repoMatcher) {
        long repoTimestamp = Long.parseLong(repoMatcher.group(2));
        return TimeUnit.MILLISECONDS.toHours(System.currentTimeMillis() - repoTimestamp) >= 24;
    }

    /**
     * Create a temporary repository for the tests.
     *
     * @param repository      - The repository base name
     * @param repoSubstitutor - Replace variables inside the repo configuration file
     * @param classLoader     - The class loader to allow read from resources
     */
    public void createRepo(TestRepository repository, StrSubstitutor repoSubstitutor, ClassLoader classLoader) {
        String repositorySettingsPath = Paths.get("integration", "settings", repository.getRepoName() + ".json").toString();
        try (InputStream inputStream = classLoader.getResourceAsStream(repositorySettingsPath)) {
            if (inputStream == null) {
                throw new IOException(repositorySettingsPath + " not found");
            }
            String repositorySettings = IOUtils.toString(inputStream, StandardCharsets.UTF_8);
            repositorySettings = repoSubstitutor.replace(repositorySettings);
            String repoKey = getRepoKey(repository);
            artifactoryClient.restCall(new ArtifactoryRequestImpl()
                    .method(ArtifactoryRequest.Method.PUT)
                    .requestType(ArtifactoryRequest.ContentType.JSON)
                    .apiUrl("api/repositories/" + repoKey)
                    .requestBody(repositorySettings));
            System.out.println("Repository " + repoKey + " created");
        } catch (Exception e) {
            fail(ExceptionUtils.getRootCauseMessage(e));
        }
    }

    /**
     * Delete the test repository.
     *
     * @param repository - The test repository to delete
     */
    public void deleteRepo(TestRepository repository) {
        String repoKey = getRepoKey(repository);
        artifactoryClient.repository(repoKey).delete();
        System.out.println("Repository " + repoKey + " deleted");
    }

    /**
     * Return true if the Artifact exists in the repository.
     *
     * @param repoKey      - Repository key
     * @param artifactName - Artifact name
     * @return true if the artifact exists in the repository
     */
    public boolean isExistInArtifactory(String repoKey, String artifactName) {
        RepositoryHandle repositoryHandle = artifactoryClient.repository(repoKey);
        try {
            repositoryHandle.file(artifactName).info();
        } catch (Exception e) {
            return false;
        }
        return true;
    }

    /**
     * Use Artifactory java client to upload a file to Artifactory.
     *
     * @param source  - Source file to upload
     * @param repoKey - Repository key
     */
    public void uploadFile(Path source, String repoKey) {
        artifactoryClient.repository(repoKey).upload(source.getFileName().toString(), source.toFile()).doUpload();
    }

    /**
     * Get build info from Artifactory.
     *
     * @param buildName   - Build name
     * @param buildNumber - Build number
     * @return build info for the specified build name and number
     * @throws IOException if failed to get the build info.
     */
    public Build getBuildInfo(String buildName, String buildNumber) throws IOException {
        return buildInfoClient.getBuildInfo(buildName, buildNumber);
    }

    /**
     * Assert that secret environment variables haven't been published.
     *
     * @param buildInfo - Build-info object
     */
    public void assertFilteredProperties(Build buildInfo) {
        Properties properties = buildInfo.getProperties();
        assertNotNull(properties);
        String[] unfiltered = properties.keySet().stream()
                .map(Object::toString)
                .map(String::toLowerCase)
                .filter(key -> StringUtils.containsAny(key, "password", "psw", "secret", "key", "token", "DONT_COLLECT"))
                .toArray(String[]::new);
        assertTrue("The following environment variables should have been filtered: " + Arrays.toString(unfiltered), ArrayUtils.isEmpty(unfiltered));
        assertTrue(properties.containsKey("buildInfo.env.COLLECT"));
    }

    /**
     * Assert that the module dependencies and the expected dependencies are equal.
     *
     * @param module               - Module to check
     * @param expectedDependencies - Expected dependencies
     */
    public void assertModuleDependencies(Module module, Set<String> expectedDependencies) {
        Set<String> actualDependencies = module.getDependencies().stream().map(Dependency::getId).collect(Collectors.toSet());
        assertEquals(expectedDependencies, actualDependencies);
    }

    /**
     * Assert that the module artifacts and the expected artifacts are equal.
     *
     * @param module            - Module to check
     * @param expectedArtifacts - Expected artifacts
     */
    public void assertModuleArtifacts(Module module, Set<String> expectedArtifacts) {
        Set<String> actualArtifacts = module.getArtifacts().stream().map(Artifact::getName).collect(Collectors.toSet());
        assertEquals(expectedArtifacts, actualArtifacts);
    }

    /**
     * Assert no artifacts in repository.
     *
     * @param repoKey - Repository key
     */
    public void assertNoArtifactsInRepo(String repoKey) {
        List<RepoPath> repoPaths = artifactoryClient.searches().repositories(repoKey).artifactsByName("*in").doSearch();
        assertTrue(repoPaths.isEmpty());
    }

    /**
     * Assert artifacts exist in repository.
     *
     * @param repoKey           - Repository key
     * @param expectedArtifacts - Expected artifacts
     */
    public void assertArtifactsInRepo(String repoKey, Set<String> expectedArtifacts) {
        List<RepoPath> repoPaths = artifactoryClient.searches().repositories(repoKey).artifactsByName("*in").doSearch();
        Set<String> actualArtifacts = repoPaths.stream().map(RepoPath::getItemPath).collect(Collectors.toSet());
        assertEquals(expectedArtifacts, actualArtifacts);
    }

    /**
     * Get module from the build-info object and assert its existence.
     *
     * @param buildInfo  - Build-info object
     * @param moduleName - Module name
     * @return module from the build-info
     */
    public Module getAndAssertModule(Build buildInfo, String moduleName) {
        assertNotNull(buildInfo);
        assertNotNull(buildInfo.getModules());
        Module module = buildInfo.getModule(moduleName);
        assertNotNull(module);
        return module;
    }

    /**
     * Assert that the artifacts and dependencies lists are not empty in the input module.
     *
     * @param buildInfo  - Build info object
     * @param moduleName - Module name
     */
    public void assertModuleContainsArtifactsAndDependencies(Build buildInfo, String moduleName) {
        Module module = getAndAssertModule(buildInfo, moduleName);
        assertTrue(CollectionUtils.isNotEmpty(module.getArtifacts()));
        assertTrue(CollectionUtils.isNotEmpty(module.getDependencies()));
    }

    /**
     * Assert that the artifacts list is not empty in the input module.
     *
     * @param buildInfo  - Build info object
     * @param moduleName - Module name
     */
    public void assertModuleContainsArtifacts(Build buildInfo, String moduleName) {
        Module module = getAndAssertModule(buildInfo, moduleName);
        assertTrue(CollectionUtils.isNotEmpty(module.getArtifacts()));
    }

    /**
     * Delete build in Artifactory.
     *
     * @param buildName - Build name to delete
     * @throws IOException if failed to execute the DELETE request or in a failure during encoding the build name
     */
    public void deleteBuild(String buildName) throws IOException {
        artifactoryClient.restCall(new ArtifactoryRequestImpl()
                .method(ArtifactoryRequest.Method.DELETE)
                .apiUrl("api/build/" + encodeBuildName(buildName))
                .addQueryParam("deleteAll", "1")
                .addQueryParam("artifacts", "1"));
    }

    private String encodeBuildName(String buildName) throws UnsupportedEncodingException {
        return URLEncoder.encode(buildName, "UTF-8").replace("+", "%20");
    }

    @Override
    public void close() {
        if (buildInfoClient != null) {
            buildInfoClient.close();
        }
        if (artifactoryClient != null) {
            artifactoryClient.close();
        }
    }
}
