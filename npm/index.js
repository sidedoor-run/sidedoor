#!/usr/bin/env node

const { spawn, spawnSync } = require('child_process')
const { existsSync, mkdirSync, chmodSync, createWriteStream } = require('fs')
const { join } = require('path')
const { homedir } = require('os')
const https = require('https')

const VERSION = require('./package.json').version
const PLATFORM = process.platform
const ARCH = process.arch

const EXT = PLATFORM === 'win32' ? '.exe' : ''
const BIN_DIR = join(homedir(), '.sidedoor', 'bin')
const BIN_PATH = join(BIN_DIR, `sidedoor${EXT}`)

function slug() {
  const os = PLATFORM === 'darwin' ? 'darwin' : PLATFORM === 'win32' ? 'windows' : 'linux'
  const arch = ARCH === 'arm64' ? 'arm64' : 'amd64'
  return `${os}_${arch}`
}

function download() {
  const url = `https://github.com/sidedoor-run/sidedoor/releases/download/v${VERSION}/sidedoor_${slug()}${EXT}`
  process.stderr.write(`  sidedoor: downloading binary...\n`)
  mkdirSync(BIN_DIR, { recursive: true })
  return new Promise((resolve, reject) => {
    const file = createWriteStream(BIN_PATH)
    function get(url) {
      https.get(url, res => {
        if (res.statusCode === 301 || res.statusCode === 302) return get(res.headers.location)
        if (res.statusCode !== 200) return reject(new Error(`HTTP ${res.statusCode} downloading binary`))
        res.pipe(file)
        file.on('finish', () => {
          file.close()
          chmodSync(BIN_PATH, 0o755)
          // macOS Sequoia sets com.apple.provenance/quarantine on files created
          // by network I/O, which causes Gatekeeper to SIGKILL the binary at
          // startup even without the quarantine flag. Strip both attributes.
          if (PLATFORM === 'darwin') {
            spawnSync('xattr', ['-d', 'com.apple.quarantine', BIN_PATH], { stdio: 'ignore' })
            spawnSync('xattr', ['-d', 'com.apple.provenance', BIN_PATH], { stdio: 'ignore' })
          }
          resolve()
        })
      }).on('error', reject)
    }
    get(url)
  })
}

function currentBinaryVersion() {
  if (!existsSync(BIN_PATH)) return null
  const { stdout, status } = spawnSync(BIN_PATH, ['--version'], { encoding: 'utf8' })
  if (status !== 0) return null
  return stdout.trim()
}

async function main() {
  if (currentBinaryVersion() !== VERSION) await download()

  const child = spawn(BIN_PATH, process.argv.slice(2), { stdio: 'inherit' })

  // Forward signals so closing the terminal or ctrl+c kills the binary too
  for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    process.on(sig, () => child.kill(sig))
  }

  child.on('exit', (code, signal) => {
    process.exit(signal ? 1 : (code ?? 1))
  })
}

main().catch(err => {
  process.stderr.write(`  sidedoor error: ${err.message}\n`)
  process.exit(1)
})
