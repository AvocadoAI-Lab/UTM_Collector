"""Exercise Linux installer transactions using fake OS commands in a private directory.

Runs on Windows with Git Bash or Linux with bash. Never installs a real service.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def fake(command, args):
    root = Path(os.environ['INSTALL_TEST_ROOT'])
    state_path = root / 'state.json'
    state = json.loads(state_path.read_text())
    stage = os.environ.get('INSTALL_TEST_FAIL', '')
    target = ''
    if command == 'install':
        target = 'config' if args[-1].endswith('/config.toml') else 'binary' if args[-1].endswith('/bin/pico-utm-agent') else ''
    elif command == 'agent':
        target = {'install': 'register', 'test': 'selftest'}.get(args[0], '')
    elif command == 'systemctl' and args[0] == 'start':
        target = 'start'
    elif command == 'ufw' and args[0] == 'allow':
        target = 'firewall'
    if stage and target == stage and not state.get('failed'):
        state['failed'] = True
        # Firewall tools can change state before reporting failure.
        if stage == 'firewall': state['firewall'] = True
        state_path.write_text(json.dumps(state))
        return 23
    rc = 0
    if command == 'id':
        if state['user']: print('1001')
        else: rc = 1
    elif command in ('useradd', 'userdel'):
        state['user'] = command == 'useradd'
    elif command == 'df':
        print('Filesystem 1024-blocks Used Available Capacity Mounted on\nfake 9999999 1 9999998 1% /')
    elif command == 'ss':
        if state.get('port_busy'): print('UNCONN 0 0 0.0.0.0:5514 0.0.0.0:* users:(("other",pid=999,fd=1))')
    elif command == 'ps': print('pico-utm-agent')
    elif command == 'runuser':
        return subprocess.call([os.environ['INSTALL_TEST_BASH'], *args[args.index('--') + 1:]])
    elif command == 'install':
        cleaned = []
        i = 0
        while i < len(args):
            if args[i] in ('-o', '-g', '-m'): i += 2
            else: cleaned.append(args[i]); i += 1
        if '-d' in cleaned:
            for path in cleaned[cleaned.index('-d') + 1:]: Path(path).mkdir(parents=True, exist_ok=True)
        else:
            shutil.copyfile(cleaned[-2], cleaned[-1])
        return 0
    elif command == 'ufw':
        if args[0] == 'status':
            print('Status: active')
            if state['firewall']: print('5514/udp ALLOW Anywhere')
        elif args[:2] == ['show', 'added']:
            if state['firewall']: print('ufw allow 5514/udp')
        elif 'delete' in args: state['firewall'] = False
        elif args[0] == 'allow': state['firewall'] = True
    elif command == 'firewall-cmd': rc = 1
    elif command == 'agent':
        if args[0] == 'install':
            (root / 'unit').write_text(f'ExecStart={root.as_posix()}/bin/pico-utm-agent run --config {root.as_posix()}/conf/config.toml\nUser=pico-utm-agent\n')
            state['enabled'] = True
        elif args[0] == 'uninstall':
            (root / 'unit').unlink(missing_ok=True)
            state['running'] = state['enabled'] = False
    elif command == 'systemctl':
        op = args[0]
        if op == 'list-unit-files':
            if (root / 'unit').exists(): print('pico-utm-agent.service enabled')
        elif op == 'is-active':
            if '--quiet' not in args: print('active' if state['running'] else 'inactive')
            rc = 0 if state['running'] else 3
        elif op == 'is-enabled':
            print('enabled' if state['enabled'] else 'disabled')
            rc = 0 if state['enabled'] else 1
        elif op == 'show': print('42')
        elif op == 'start': state['running'] = True
        elif op == 'stop': state['running'] = False
        elif op == 'enable': state['enabled'] = True
        elif op == 'disable': state['enabled'] = False; state['running'] = False
    state_path.write_text(json.dumps(state))
    return rc


def main():
    bash = shutil.which('bash') if os.name != 'nt' else r'C:\Program Files\Git\bin\bash.exe'
    cache = ROOT / '.cache'
    cache.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='install-test-', dir=cache) as tmp:
        suite = Path(tmp)
        for case in ('fresh', 'force', 'uninstall', 'occupied', 'invalid-id', 'binary', 'config', 'firewall', 'register', 'selftest', 'start', 'force-selftest', 'force-start'):
            root = suite / case
            root.mkdir()
            (root / 'bin').mkdir()
            (root / 'tmp').mkdir()
            shims = root / 'shims'
            shims.mkdir()
            existing = case.startswith('force') or case in ('uninstall', 'invalid-id')
            state = dict(user=existing, running=existing, enabled=existing, firewall=existing, port_busy=case == 'occupied')
            (root / 'state.json').write_text(json.dumps(state))
            python = Path(sys.executable).as_posix()
            dispatcher = Path(__file__).as_posix()
            for command in ('id', 'useradd', 'userdel', 'df', 'ss', 'ps', 'runuser', 'install', 'chown', 'chmod', 'sleep', 'ufw', 'firewall-cmd', 'systemctl', 'journalctl', 'agent'):
                path = shims / command
                path.write_text(f'#!/usr/bin/env bash\nexec "{python}" "{dispatcher}" --fake {command} "$@"\n', encoding='utf-8', newline='\n')
                path.chmod(0o755)
            shutil.copyfile(shims / 'agent', root / 'agent')
            replacements = {
                '/usr/local/bin/pico-utm-agent': f'{root.as_posix()}/bin/pico-utm-agent',
                '/etc/pico-utm-agent': f'{root.as_posix()}/conf',
                '/var/lib/pico-utm-agent': f'{root.as_posix()}/data',
                '/var/log/pico-utm-agent': f'{root.as_posix()}/logs',
                '/etc/systemd/system/$SERVICE.service': f'{root.as_posix()}/unit',
                '/var/tmp/pico-utm-agent-install.XXXXXXXX': f'{root.as_posix()}/tmp/backup.XXXXXXXX',
                '[[ $EUID -eq 0 ]]': 'true',
                'for p in /usr/local/bin /etc /var/lib /var/log; do': f'for p in "{root.as_posix()}"; do',
                'df -Pk /var/lib': f'df -Pk "{root.as_posix()}"',
                'cat /proc/sys/kernel/random/uuid': "printf '87654321-1234-1234-1234-123456789abc'",
            }
            for script in ('install.sh', 'uninstall.sh'):
                text = (ROOT / 'scripts' / script).read_text(encoding='utf-8')
                for old, new in replacements.items(): text = text.replace(old, new)
                (root / script).write_text(text, encoding='utf-8', newline='\n')
            agent_id = '12345678-1234-1234-1234-123456789abc'
            original_config = f'[agent]\nagent_id = "{agent_id}"\n[listener]\nport = 5514\n'
            if existing:
                for name in ('conf', 'data/spool', 'logs'): (root / name).mkdir(parents=True, exist_ok=True)
                (root / 'conf/config.toml').write_text(original_config if case != 'invalid-id' else 'invalid')
                (root / 'data/spool/customer-data').write_text('must survive')
                shutil.copyfile(root / 'agent', root / 'bin/pico-utm-agent')
                (root / 'unit').write_text(f'ExecStart={root.as_posix()}/bin/pico-utm-agent run --config {root.as_posix()}/conf/config.toml\nUser=pico-utm-agent\n')
            env = os.environ.copy()
            env.update(INSTALL_TEST_ROOT=str(root), INSTALL_TEST_BASH=str(bash), INSTALL_TEST_FAIL=case.removeprefix('force-') if case not in ('fresh', 'force', 'uninstall', 'occupied', 'invalid-id') else '')
            # Convert paths in bash itself; Windows drive letters are not PATH separators.
            command = 'export PATH="$(cygpath -u "$INSTALL_TEST_ROOT")/shims:$PATH"; exec bash "$@"' if os.name == 'nt' else 'export PATH="$INSTALL_TEST_ROOT/shims:$PATH"; exec bash "$@"'
            args = [str(root / ('uninstall.sh' if case == 'uninstall' else 'install.sh'))]
            if case != 'uninstall': args += ['--endpoint', 'https://example.test', '--token', 'test-token', '--site-id', 'test-site'] + (['--force'] if existing else [])
            result = subprocess.run([str(bash), '-c', command, 'test', *args], env=env, capture_output=True, timeout=60)
            (root / 'output.log').write_bytes(result.stdout + result.stderr)
            after = json.loads((root / 'state.json').read_text())
            try:
                success = case in ('fresh', 'force', 'uninstall')
                assert (result.returncode == 0) == success, f'exit {result.returncode}'
                if case in ('fresh', 'force'):
                    assert after['running'] and after['enabled'] and after['firewall']
                    if existing: assert agent_id in (root / 'conf/config.toml').read_text()
                elif case == 'uninstall':
                    assert not after['running'] and not after['enabled'] and not after['firewall']
                    assert not (root / 'unit').exists() and not (root / 'bin/pico-utm-agent').exists()
                    assert (root / 'conf/config.toml').read_text() == original_config
                else:
                    if case not in ('occupied', 'invalid-id'): assert after.get('failed'), 'failure injection was not reached'
                    for key in ('running', 'enabled', 'firewall', 'user'): assert after[key] == state[key], key
                    if existing and case != 'invalid-id': assert (root / 'conf/config.toml').read_text() == original_config
                    if not existing:
                        assert not (root / 'unit').exists() and not (root / 'bin/pico-utm-agent').exists()
                        assert not (root / 'conf').exists() and not (root / 'data').exists() and not (root / 'logs').exists()
                    assert not list((root / 'tmp').iterdir()), 'rollback snapshot leaked'
                if existing: assert (root / 'data/spool/customer-data').read_text() == 'must survive'
            except AssertionError as error:
                print(result.stdout.decode('utf-8', errors='replace'))
                print(result.stderr.decode('utf-8', errors='replace'))
                raise AssertionError(f'{case}: {error}') from error
            print(f'PASS installer {case}', flush=True)


if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == '--fake': sys.exit(fake(sys.argv[2], sys.argv[3:]))
    main()
