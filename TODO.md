# TODO

* Use Cobra as CLI line framework.
* Add code coverage in the CI. Add a badge in the README.
* Use yaml config instead of env variables. For secrets, read them from file. The config should reference the file paths.
* Create a makefile for the project.
* Each container has its network, we set the hostname simply to see the proper name of the box in Claude code on the web. However, I think we should have a concept of box id instead of hostname from the code interfaces. The hostname is set to the box id, that's it.
* There are many functions where the errors are inhibited. This is bad, we should never inhibit a function. We need to fix that.
* We want to pin the version of the Claude code binary in the repo. Then we want to create a github action that resolve the current version and create a PR to bump the version.
* We want to support remote docker spokes, not only the hub running on the server. Basically llmbox should be the frontend and we should have a way to make spokes join the cluster. Consequently, we should have an option to tell on which spoke to create the container on. In order to simplify, we want an option to make the main server both a server and a spoke so that we don't need to deploy two components in simple setups.