"""Smoke test the built binaries over real loopback UDP/HTTPS, without installing services."""
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent


def main():
    target = 'windows' if os.name == 'nt' else 'linux'
    ext = '.exe' if os.name == 'nt' else ''
    binaries = ROOT / 'dist' / f'pico-utm-agent-{target}-amd64'
    with tempfile.TemporaryDirectory(prefix='runtime-', dir=ROOT / '.cache') as tmp:
        work = Path(tmp)
        processes = []
        logs = []
        def launch(name, *args):
            log = open(work / f'{name}.console.log', 'wb')
            logs.append(log)
            proc = subprocess.Popen([str(binaries / (name + ext)), *args], stdout=log, stderr=log,
                                    creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
            processes.append(proc)
            return proc
        def port(kind):
            with socket.socket(socket.AF_INET, kind) as sock:
                sock.bind(('127.0.0.1', 0))
                return sock.getsockname()[1]
        def wait(check, seconds=75):
            deadline = time.monotonic() + seconds
            while time.monotonic() < deadline:
                try:
                    result = check()
                    if result: return result
                except (OSError, ValueError): pass
                time.sleep(.2)
            raise AssertionError('Timed out waiting for runtime check')
        token = 'runtime-' + uuid.uuid4().hex
        tcp, udp = port(socket.SOCK_STREAM), port(socket.SOCK_DGRAM)
        endpoint = f'https://127.0.0.1:{tcp}'
        ca = work / 'certs/mock-ca.pem'
        try:
            launch('mockserver', '-addr', f'127.0.0.1:{tcp}', '-token', token, '-cert-dir', str(ca.parent))
            wait(ca.exists)
            context = ssl.create_default_context(cafile=str(ca))
            def get(path):
                req = urllib.request.Request(endpoint + path, headers={'Authorization': 'Bearer ' + token})
                with urllib.request.urlopen(req, context=context, timeout=3) as response:
                    return json.load(response)
            wait(lambda: get('/healthz')['ok'])
            agent_id = str(uuid.uuid4())
            config = work / 'config.toml'
            config.write_text(f'''[agent]
agent_id = "{agent_id}"
site_id = "runtime-test"
[listener]
addr = "127.0.0.1"
port = {udp}
[forwarder]
endpoint = "{endpoint}"
token = "{token}"
batch_size = 500
batch_interval_seconds = 5
ca_file = '{ca}'
[heartbeat]
interval_seconds = 60
[spool]
dir = '{work / 'spool'}'
retention_days = 30
max_size_gb = 10
[logging]
level = "debug"
dir = '{work / 'logs'}'
[proxy]
url = ""
''', encoding='utf-8')
            agent = launch('agent', 'run', '--config', str(config))
            status = work / 'status.json'
            wait(lambda: status.exists() and get('/_control/stats')['heartbeats'] > 0)
            result = subprocess.run([str(binaries / ('syslog-gen' + ext)), '-target', f'127.0.0.1:{udp}', '-n', '1000', '-rate', '250', '-start-id', '123456700000000'], capture_output=True, timeout=15)
            assert result.returncode == 0
            wait(lambda: get('/_control/stats')['events_accepted'] == 1000)
            events = get('/_control/events')
            assert {event['event_id'] for event in events} == {str(123456700000000 + i) for i in range(1000)}
            def complete_status():
                data = json.loads(status.read_text(encoding='utf-8'))
                return data if data['events_forwarded_total'] == 1000 else None
            data = wait(complete_status)
            assert data['agent_id'] == agent_id and data['events_received_total'] == 1000
            for key in ('events_dropped_total', 'events_dead_lettered_total', 'spool_write_errors_total', 'spool_backlog_bytes'):
                assert data[key] == 0, key
            assert token not in status.read_text(encoding='utf-8')
            files = list((work / 'logs').glob('agent-*.log'))
            assert files and all(token not in path.read_text(encoding='utf-8') for path in files)
            for command in ('logs', 'status'):
                output = subprocess.run([str(binaries / ('agent' + ext)), command, '--config', str(config)], capture_output=True, timeout=10)
                assert output.returncode == 0, command
                assert str(files[0]).encode() in output.stdout, 'missing absolute log path'
            assert agent.poll() is None
            print('PASS runtime: 1000 exact event IDs, unique stats, status counters, token redaction, logs/status commands', flush=True)
        finally:
            for process in reversed(processes):
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=10)
            for log in logs: log.close()


if __name__ == '__main__': main()
