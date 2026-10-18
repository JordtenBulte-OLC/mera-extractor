# mera-extractor
Uses `mxcli` alpha project by `Mendix Labs` and actual Mendix binaries to extract changes and analyze them

# Implementing new Mendix version
`./scrips/fetch-mx.sh` is created to extract and trim actual Mendix binaries that are use for `mx diff`

# known issues
- Works for diff on the same Mendix version. Only a small subset of Mendix version is supported (see mx-versions.txt)
- Diffing and analyzing does not work for version upgrade commits
# Running it locally (Ubuntu on WSL2 → Windows)
All commands run from this folder (`~/mera/extractor`) in the Ubuntu shell.
`docker-compose.yml` publishes container port `8080` on host port **`8081`**
(8080/8090 are taken by Studio Pro), and WSL2 forwards `localhost` to Windows,
so the Mendix app on Windows reaches it at **http://localhost:8081**.

```bash
# start Docker if it isn't running yet (Docker Engine inside WSL)
sudo service docker start

# build (if needed) and start in the background
docker compose up -d --build     # drop --build to reuse the existing image

# check it's up — from Ubuntu, or from a browser/PowerShell on Windows
curl http://localhost:8081/health
```

Day-to-day:

```bash
docker compose ps                # status
docker compose logs -f           # follow container output (also ./logs/extractor.log)
docker compose restart           # restart without rebuilding
docker compose down              # stop and remove the container
docker compose down -v           # ...and also drop the mera-work clone volume
```

Debug build (delve on `127.0.0.1:2345`; process waits until the IDE attaches and continues):

```bash
docker compose -f docker-compose.yml -f docker-compose.debug.yml up --build
```

If Windows can't reach `localhost:8081`, check that WSL localhost forwarding is
on (it is by default; `localhostForwarding=true` in `%UserProfile%\.wslconfig`),
or use the WSL IP from `hostname -I` instead.
