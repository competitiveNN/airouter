#!/usr/bin/env python3
"""End-to-end test script making a successful request to OpenCode through airouter / proxy."""
import subprocess
import time
import urllib.request
import json
import os
import signal
import socket

def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]

def wait_for_port(port, timeout=10):
    start = time.time()
    while time.time() - start < timeout:
        try:
            with socket.create_connection(('127.0.0.1', port), timeout=0.5):
                return True
        except (socket.error, ConnectionRefusedError):
            time.sleep(0.1)
    return False

def main():
    up_port = free_port()
    gateway_port = free_port()

    # 1. Start fake OpenCode upstream
    upstream_proc = subprocess.Popen(
        ['python3', 'scripts/fake-opencode-upstream.py', '--port', str(up_port)],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE
    )
    if not wait_for_port(up_port):
        print("Failed to start fake upstream")
        upstream_proc.kill()
        return

    # 2. Write temporary config pointing smart profile to fake upstream opencode model
    cfg_content = f"""
providers:
  opencode:
    url: http://127.0.0.1:{up_port}/v1
    api_key_env: OPENCODE_API_KEY

models:
  smart:
    chain:
      - provider: opencode
        model: space-bunny-free
        vision: false
        intelligence: 25.0
  work:
    chain:
      - provider: opencode
        model: space-bunny-free
        vision: false
        intelligence: 25.0
  fast:
    chain:
      - provider: opencode
        model: space-bunny-free
        vision: false
        intelligence: 5.0
  large:
    chain:
      - provider: opencode
        model: space-bunny-free
        vision: true
        intelligence: 10.0
        context_length: 200000
"""
    cfg_path = '/tmp/test-opencode-config.yaml'
    with open(cfg_path, 'w') as f:
        f.write(cfg_content)

    # 3. Start airouter gateway
    env = os.environ.copy()
    env['OPENCODE_API_KEY'] = 'fake-key-for-test'
    env['GATEWAY_API_KEY'] = ''
    env['ALLOW_NO_AUTH'] = '1'

    gateway_proc = subprocess.Popen(
        ['./airouter', '-config', cfg_path, '-port', str(gateway_port), '-allow-no-auth'],
        env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE
    )
    if not wait_for_port(gateway_port):
        print("Failed to start airouter gateway")
        upstream_proc.kill()
        gateway_proc.kill()
        return

    try:
        # 4. Make request through airouter proxy to OpenCode model
        url = f"http://127.0.0.1:{gateway_port}/v1/chat/completions"
        payload = {
            "model": "smart",
            "messages": [{"role": "user", "content": "hello from test script"}],
            "stream": False
        }
        req = urllib.request.Request(
            url,
            data=json.dumps(payload).encode('utf-8'),
            headers={'Content-Type': 'application/json'}
        )
        print(f"Making request to airouter at {url}...")
        with urllib.request.urlopen(req, timeout=10) as resp:
            status = resp.status
            body = resp.read().decode('utf-8')
            print(f"Response status: {status}")
            print(f"Response body: {body}")
            data = json.loads(body)
            assert 'choices' in data, "Response missing choices"
            print("SUCCESS: Request successfully routed to OpenCode and completed with 200 OK!")
    except Exception as e:
        print(f"Request failed: {e}")
        gateway_proc.kill()
        upstream_proc.kill()
        raise
    finally:
        gateway_proc.terminate()
        upstream_proc.terminate()
        gateway_proc.wait()
        upstream_proc.wait()

if __name__ == '__main__':
    main()
