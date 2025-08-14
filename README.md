
# dropbox-sync

Small Go utility to synchronize a local folder to a Dropbox folder.

This README explains how to build and run the program, what configuration it expects, environment variables and available flags.

## Prerequisites

- Go (1.18+ recommended) installed and on your PATH to build from source. On Windows you can use PowerShell.
- A Dropbox API access token with file access.

## Configuration

The program accepts configuration from (in order of precedence): command-line flags, environment variables, or a JSON config file.

- Example config: `dropbox-sync.example.json` — copy this to `dropbox-sync.json` and edit values as needed.
- Config file format (JSON):

	{
		"access_token": "<DROPBOX_TOKEN>",
		"local_folder": "C:/path/to/local/folder",
		"remote_folder": "/Apps/MyApp/Folder",
		"delete": false,
		"download": false,
		"mode": "two-way",
		"workers": 8
	}

Fields:
- `access_token` (string) — Dropbox access token.
- `local_folder` (string) — path to the local folder to sync.
- `remote_folder` (string) — Dropbox folder path (must start with `/`).
- `delete` (bool, optional) — delete remote files not present locally (one‑way mirror behavior when enabled).
- `download` (bool, optional) — enable downloading remote files that are missing locally or whose remote copy is newer.
- `mode` (string, optional) — alternative to separate flags: `upload`, `download`, `two-way`, `mirror`.
- `workers` (int, optional) — number of parallel workers.

## Environment variables

These environment variables are also recognized (used when flags and config file are not present):

- `DBSYNC_ACCESS_TOKEN` or `DROPBOX_ACCESS_TOKEN` — access token
- `DBSYNC_LOCAL_FOLDER` or `LOCAL_FOLDER` — local folder path
- `DBSYNC_REMOTE_FOLDER` or `DROPBOX_FOLDER` — remote Dropbox folder

## Command-line flags

Important flags the program accepts:

- `--config` : Path to a JSON config file (default: none; `dropbox-sync.json` is auto-loaded if present)
- `--token` : Dropbox access token (overrides env & config)
- `--local` : Local folder path (overrides env & config)
- `--remote` : Remote Dropbox folder (must start with `/`, overrides env & config)
- `--delete` : Enable deletions of remote files not present locally
- `--download` : Enable downloading remote files missing locally or whose remote copy is newer
- `--mode` : Combined directional modes: upload|download|two-way|mirror (overrides --delete/--download)
- `--workers` : Number of parallel workers (defaults to min(8, CPU*2))
- `--dry-run` : Show actions without uploading/deleting
- `-v` : Verbose logging
- `--no-progress` : Disable the progress bar output
- `--bar-width` : Progress bar width (characters)

Run `dropbox-sync --help` for the full list and descriptions.

## Build

From the repository root:

On Windows PowerShell:

```powershell
go build -o .\dropbox-sync.exe
```

Or build a platform-native binary with:

```powershell
go build
```

## Run

Typical workflows:

- Using a config file (recommended):

	1. Copy the example config:

		 ```powershell
		 copy .\dropbox-sync.example.json .\dropbox-sync.json
		 # Edit dropbox-sync.json and set access_token, local_folder, and remote_folder
		 ```

	2. Run the program (after building):

		 ```powershell
		 .\dropbox-sync.exe --config .\dropbox-sync.json
		 ```

- Using flags and environment variables:

	```powershell
	# using flags
	.\dropbox-sync.exe --token "<TOKEN>" --local "C:\path\to\local" --remote "/Apps/MyApp/Folder" --dry-run

	# or using environment variables
	$env:DBSYNC_ACCESS_TOKEN = '<TOKEN>'
	$env:DBSYNC_LOCAL_FOLDER = 'C:\path\to\local'
	$env:DBSYNC_REMOTE_FOLDER = '/Apps/MyApp/Folder'
	.\dropbox-sync.exe --dry-run
	```

Notes:
- Remote folder must start with `/`.
- Use `--dry-run` first to preview actions.
- `--delete` removes remote files not present locally.
- `--download` also pulls missing or newer remote files locally.

## Examples

```powershell
# Dry run
.\dropbox-sync.exe --config .\dropbox-sync.json --dry-run

# Real run with deletions enabled
.\dropbox-sync.exe --config .\dropbox-sync.json --delete

# Two-way (adds remote downloads of newer/missing files)
.\dropbox-sync.exe --config .\dropbox-sync.json --download
```

## Troubleshooting

- If you see "Dropbox access token not provided" the program could not find a token via flag, env or config file.
- If remote listing fails the program will treat the remote as empty and proceed to upload local files.

## License

This project is licensed under the MIT License. See `LICENSE` for details.
