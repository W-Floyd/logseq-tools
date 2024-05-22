# Logseq Tools

Collection of utilities for creating/consuming Logseq data to/from various sources.

## Jira

Pulls issues from Jira and creates Logseq pages for them (one way sync)
Will overwrite any existing pages at the file name, so don't edit these files - they are meant to be referenced only.

Get your API key [here](https://id.atlassian.com/manage-profile/security/api-tokens). Username is typically your email.

See `config.example.json` for the file format to expect.

### API Calls
If you have many issues, you may run into rate limiting.
I have not experienced this in normal use so far, only when running multiple times quickly.
Config options which may help reduce API calls are:

```json
"include_watchers": false, // Saves 1 extra API call per Issue
"include_comments": false, // Saves 1 extra API call per Issue
"include_done": false // Skips an Issue if done, saves up to 2 API calls per done Issue. No savings if include_watchers and include_comments are false.
```

The tool does handle handle API rate limiting by pausing until the API returned retry time `X-RateLimit-Reset` ([Docs](https://developer.atlassian.com/cloud/jira/platform/rate-limiting/)) and retrying the failed query.

### Logseq slowdown
It is recommended to have the following settings to prevent Logseq slowdowns when viewing graphs:

```json
"exclude_from_graph": true // Adds 'exclude-from-graph-view:: true' to each page, greatly cleaning up the graph page
```