// PM2 config untuk codex-bridge (Go binary).
// interpreter: "none" wajib untuk binary Go — tanpa itu PM2 membungkusnya
// dengan node dan proses langsung mati dengan SyntaxError.
module.exports = {
  apps: [
    {
      name: 'codex-bridge',
      cwd: '/home/unity008/.workspace/codex-bridge',
      script: './codex-bridge',
      interpreter: 'none',
      args: '-config /home/unity008/.workspace/codex-bridge/config.yaml',
      autorestart: true,
      max_restarts: 10,
      restart_delay: 2000,
      kill_timeout: 5000,
      merge_logs: true,
      time: true,
      env: { GOMAXPROCS: '1' }
    }
  ]
};
