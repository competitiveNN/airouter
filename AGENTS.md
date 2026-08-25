Build a ai gateway which main job is expose four models (smart, work, fast, large)
- smart: the most intelligent
- work: the "workhorse" that does the coding
- fast: for small, quick or verbose tasks that are simple enough for a small model
- large: for tasks that require very large context windows and for context compression

The gateway plugs into as many open ai compatible endpoints and exposes an open ai compatible endpoint.
Each of the 4 models can be configured to automatically route to a predefined list of models (fallback chain).
The sessions are ofc persistent, and same session are always routed to the same model.
If api rate limits are met mid session, the context is moved over to the next model in the fallback chain list.
Models that incur in errors receive a cooldown period. Cooldown time is based on error time (e.g. 429 have low cooldown, 404 have high cooldown, and so on)
We must account for all kind of possible errors, we want to catch any error that can happen even mid streaming the response, such that even during stream we can catch the error, move the session to the next model and continue streaming, such that the client never experiences any error. (from the point of the client the stream would experience just a slightly delay while the router waits for the next model in the chain to resume streaming).
Authentication is performed using env var api keys. Custom providers with custom urls can be defined and the configuration is stored and read from storage.
