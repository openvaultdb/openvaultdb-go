#!/usr/bin/env node
// Interop check: does @ingitdb/client-github's REAL collection-loading code
// (loadCollectionSchema/loadCollectionRecords, unmodified) understand the
// on-disk layout dalgo2ingitdb writes (.ingitdb/root-collections.yaml,
// <collection>/.collection/definition.yaml, <collection>/$records/*.json)?
//
// This is the format-compatibility half of the "sneat-go writes with
// dalgo2ingitdb, the browser reads with ingitdb-ts" thesis. It could not be
// run against a REAL github.com repo in the PoC session that produced this
// script (creating a GitHub repo requires the user's own direct request, not
// a coordinator-relayed instruction — see the PR description), so it swaps
// only the TRANSPORT: a fake GithubApi that reads the fixture from local disk
// instead of https://api.github.com. The parsing/path-resolution logic under
// test is imported unmodified from @ingitdb/client + @ingitdb/client-github.
//
// Usage:
//   1. go run ./cmd/write-fixture /tmp/ingitdb-fixture
//   2. npm install @ingitdb/client @ingitdb/client-github   (in some scratch dir)
//   3. node scripts/ts-read-check.mjs /tmp/ingitdb-fixture
//
// (During the PoC session this instead imported the two packages by absolute
// path from a local ingitdb-ts checkout's built dist/, to avoid installing
// into this Go-module directory; either import style exercises the same
// published code.)
import { readFile, readdir } from 'node:fs/promises'
import path from 'node:path'
import { createCache } from '@ingitdb/client'
import { loadCollectionSchema, loadCollectionRecords } from '@ingitdb/client-github'

const fixtureRoot = process.argv[2]
if (!fixtureRoot) {
  console.error('usage: node ts-read-check.mjs <fixture-dir>')
  process.exit(2)
}

const REPO = 'owner/example-vault' // repo name is irrelevant to the fake transport below

/** @type {import('@ingitdb/client-github').GithubApi} */
const fsGithubApi = {
  async getFileText(_repo, filePath) {
    const decodedContent = await readFile(path.join(fixtureRoot, filePath), 'utf8')
    return { decodedContent }
  },
  async getContents(_repo, dirPath) {
    const entries = await readdir(path.join(fixtureRoot, dirPath), { withFileTypes: true })
    return entries.map((e) => ({
      type: e.isDirectory() ? 'dir' : 'file',
      name: e.name,
      path: path.posix.join(dirPath, e.name)
    }))
  }
}

const cache = createCache()

const { schema, collectionPath } = await loadCollectionSchema(REPO, 'main', 'contacts', {
  githubApi: fsGithubApi,
  cache
})

const { records } = await loadCollectionRecords(REPO, 'main', 'contacts', schema, collectionPath, {
  githubApi: fsGithubApi,
  cache
})

const jane = records.find((r) => r._id === 'contact-jane-doe')
if (!jane) {
  console.error('FAIL: contact-jane-doe not found in records loaded via @ingitdb/client-github')
  console.error('records:', records)
  process.exit(1)
}
if (jane.firstName !== 'Jane' || jane.lastName !== 'Doe' || jane.email !== 'jane@example.com') {
  console.error('FAIL: field mismatch', jane)
  process.exit(1)
}
if (!Array.isArray(jane.roles) || jane.roles.length !== 2 || jane.roles[0] !== 'owner' || jane.roles[1] !== 'driver') {
  console.error('FAIL: roles mismatch', jane.roles)
  process.exit(1)
}
console.log('PASS: @ingitdb/client-github correctly read the contactus-shaped record dalgo2ingitdb wrote.')
console.log(JSON.stringify({ schema, collectionPath, records }, null, 2))
