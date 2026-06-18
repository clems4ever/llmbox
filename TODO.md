# TODO

* We want to support remote docker spokes, not only the hub running on the server. Basically llmbox should be the frontend and we should have a way to make spokes join the cluster. Consequently, we should have an option to tell on which spoke to create the container on. In order to simplify, we want an option to make the main server both a server and a spoke so that we don't need to deploy two components in simple setups.
* Log all commands run in the container in a way that Claude cannot prevent the recording in any way.
* We should create an integration test that execute the code from start to finish with a webdriver and emulate the Anthropic backend obviously.
* We want to have a flag to enforce authentication of the user before being able to activate a box. This is so that nobody can register the box before on behalf of who requested the creation.
* Paginate container listing.