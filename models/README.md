# Local model files

Model weights are downloaded separately and ignored by Git. The default model is
Qwen2.5-1.5B-Instruct, quantized as Q4_K_M (about 1.12 GB).

From the project root, download it with Python 3 (no extra packages required):

```bash
python3 models/download.py
```

The script saves `qwen2.5-1.5b-instruct-q4_k_m.gguf` in this directory regardless
of your working directory. It skips an existing file and removes partial files
after a failed or interrupted download; rerun it to retry.

For a manual download, open the
[official Qwen model repository](https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF)
or use the [direct GGUF download](https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf).
Save it as `models/qwen2.5-1.5b-instruct-q4_k_m.gguf` without changing the filename.

After installing the llama.cpp runtime described in the [project setup](../README.md#setup), run:

```bash
python cli.py models start local-llm
```

For a different model, obtain its weights from its publisher and update
`launch.model_file` and the registration in `gateway.config.json` as described in
the [model guide](../README.models.md). The download script only fetches the default
model. Files and subdirectories here are ignored except this README and the script.
