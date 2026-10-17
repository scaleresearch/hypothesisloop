# The image every job runs in: sh and git, which the launch needs, and CPU builds of torch and
# torchvision from PyTorch's CPU index: PyPI's torch brings CUDA, gigabytes no job here uses.
FROM python:3.13-slim
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cpu torch==2.11.0 torchvision==0.26.0
