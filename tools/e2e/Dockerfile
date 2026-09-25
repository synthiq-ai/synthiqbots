# Runs the E2E harness from the deploy workflow's runner container (which has
# no pip/ensurepip). Stdlib only — SRP6 is pure Python (wowclient/srp.py). Host networking reaches authserver/worldserver on 127.0.0.1;
# the docker socket + static CLI let the harness read logs / query the DB.
FROM docker:27-cli AS dockercli
FROM python:3.12-slim
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
WORKDIR /e2e
ENTRYPOINT ["python", "run.py"]
