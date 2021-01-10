package com.jfrog.testing;

/**
 * @author yahavi
 */
@SuppressWarnings("unused")
public class TestRepository {
    public enum RepoType {
        LOCAL,
        REMOTE,
        VIRTUAL
    }

    private final String repoName;
    private final RepoType repoType;

    public TestRepository(String repoName, RepoType repoType) {
        this.repoName = "jfrog-rt-tests-" + repoName;
        this.repoType = repoType;
    }

    public RepoType getRepoType() {
        return repoType;
    }

    public String getRepoName() {
        return repoName;
    }

    @Override
    public String toString() {
        return getRepoName();
    }
}
