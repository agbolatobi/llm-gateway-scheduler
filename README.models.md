# Model management

`gateway.config.json` is shared by Go and Python. Registration is explicit.

```bash
python cli.py models validate
python cli.py models list
python cli.py models status
python cli.py models start local-llm
python cli.py models stop local-llm
```

Use the [shared venv](README.md#setup). Commands accept `--config PATH`;
start/stop also accept `--timeout SECONDS`. Start verifies identity/context;
stop signals only recorded, owned processes. List/validate make no network calls.

## Add a model

1. Obtain its GGUF or start an external llama.cpp server.
2. Register unique model/alias/runtime IDs and a backend port.
3. Set expected identity, context/output limits and quotas.
4. Add `launch` for CLI-managed models (`parallel: 1`); paths resolve from the
   config directory. Omit `launch` for external models.
5. Validate, start explicitly, then run `./.runtime/gateway reload`.

Hardware determines capacity. [Reload](gateway/README.md#reload) preserves usage
but never starts/stops models; existing identity/context/launch changes need restart.

## Check an unregistered server

```bash
python cli.py models check --base-url http://127.0.0.1:8082
```

This checks one address and warns without editing configuration. Direct chat is
still possible. Chat `--config` does not select its URL. Legacy `--start-server`
uses fixed weights; `models start` uses registrations.

Any valid API key can use any registered model. Listing does not prove readiness.
Token counts include the chat template and fail closed. External model identity
is self-reported; managed model paths are also checked.

Errors: unknown model **404**, invalid budget **400**, quota **429**, unavailable
runtime **503**. [Accounting](gateway/README.lifecycle.md).

Test: `python -m unittest discover -s tests -p test_models.py -v`.
