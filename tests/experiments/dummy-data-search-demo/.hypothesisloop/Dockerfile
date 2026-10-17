# The image every job runs in: sh and git, which the launch needs, and Python with numpy.
FROM python:3.13-slim
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir numpy==2.3.3
