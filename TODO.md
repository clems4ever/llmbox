# TODO

* Use Cobra as CLI line framework.
* Add code coverage in the CI. Add a badge in the README.
* Use yaml config instead of env variables. For secrets, read them from file. The config should reference the file paths.
* Create a makefile for the project.
* Each container has its own network so setting hostname does not really make sense. We should rather use some kind of id and make sure the operations can reach only containers managed by llmbox, not other containers!
* There are many functions where the errors are inhibited. This is bad, we should never inhibit a function. We need to fix that.
* We want to pin the version of the Claude code binary in the repo. Then we want to create a github action that resolve the current version and create a PR to bump the version.