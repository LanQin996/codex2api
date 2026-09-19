import { createServer } from 'vite'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

export async function startGalleryFixture() {
  process.env.VITE_APP_VERSION ||= 'test'
  const cwd = fileURLToPath(new URL('../../', import.meta.url))
  const accounts = Array.from({ length: 125 }, (_, i) => ({
    id: i + 1,
    name: 'Account ' + String(i + 1).padStart(3, '0'),
    email: 'account' + (i + 1) + '@example.test',
    plan_type: i % 2 ? 'plus' : 'pro',
    status: 'active',
    channel: 'codex',
  }))
  const output =
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 420 280"><rect width="420" height="280" fill="#f4f0dd"/><g fill="none" stroke="#314b51" stroke-width="5"><circle cx="110" cy="195" r="55"/><circle cx="320" cy="195" r="55"/><path d="M110 195 175 120 220 195 110 195 265 120 320 195M175 120H265" stroke="#23939b"/></g><path d="M150 100Q105 45 160 55Q200 65 230 85Q255 85 250 45Q245 20 265 22Q290 22 280 55L275 97Q220 142 150 100" fill="#fffdf1" stroke="#314b51" stroke-width="3"/><path d="M270 42 380 48Q310 91 270 52Z" fill="#f5bd49" stroke="#314b51" stroke-width="3"/><circle cx="266" cy="33" r="3" fill="#314b51"/><path d="M190 114 204 153 190 194M218 114 233 155 245 193" fill="none" stroke="#e7a937" stroke-width="7"/></svg>'
  let jobs = accounts.slice(0, 25).map((a, i) => ({
    id: i + 1,
    account_id: a.id,
    account_name: a.name,
    plan_type: a.plan_type,
    channel: 'codex',
    model: 'gpt-5.5',
    reasoning_effort: 'high',
    preset_kind: 'builtin',
    preset_ref: 'pelican',
    preset_name: 'Pelican',
    status: i === 3 ? 'error' : 'completed',
    error: i === 3 ? 'Fixture upstream failure' : '',
    created_at: '2026-09-19T12:00:00Z',
    updated_at: '2026-09-19T12:00:02Z',
    duration_ms: 1200 + i * 100,
    output: i === 4 ? 'No markup returned' : output,
    prompt: 'Snapshot prompt',
  }))
  const batches = new Map()
  const submissions = []
  let peak = 0,
    active = 0,
    detailRequests = 0
  const backend = await readFile(
    new URL('../../../admin/quality_test_handler.go', import.meta.url),
    'utf8',
  )
  const preview = backend.slice(
    backend.indexOf('<!doctype html>'),
    backend.indexOf('</html>`))') + 7,
  )
  const server = await createServer({
    root: cwd,
    server: { host: '127.0.0.1', port: 0 },
    plugins: [
      {
        name: 'quality-test-fixture',
        configureServer(server) {
          server.middlewares.use(async (req, res, next) => {
            const url = new URL(req.url, 'http://fixture')
            if (!url.pathname.startsWith('/api/')) return next()
            const json = (data, status = 200) => {
              res.statusCode = status
              res.setHeader('Content-Type', 'application/json')
              res.end(JSON.stringify(data))
            }
            const body = async () => {
              let s = ''
              for await (const chunk of req) s += chunk
              return JSON.parse(s || '{}')
            }
            if (url.pathname === '/api/quality-test/preview') {
              res.setHeader('Content-Type', 'text/html')
              res.setHeader(
                'Content-Security-Policy',
                "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; connect-src 'none'; frame-src 'none'; object-src 'none'; sandbox allow-scripts",
              )
              res.end(preview)
              return
            }
            const path = url.pathname.replace('/api/admin', '')
            if (path === '/bootstrap-status')
              return json({ needs_bootstrap: false, source: 'env' })
            if (path === '/health') return json({ status: 'ok' })
            if (path === '/settings/visible-channels')
              return json({ channels: ['codex'] })
            if (path === '/accounts') {
              const matched = accounts.filter((a) =>
                (a.name + ' ' + a.email)
                  .toLowerCase()
                  .includes(
                    (url.searchParams.get('search') || '').toLowerCase(),
                  ),
              )
              const page = Number(url.searchParams.get('page') || 1),
                size = Number(url.searchParams.get('page_size') || 20)
              return json({
                accounts: matched.slice((page - 1) * size, page * size),
                total: matched.length,
              })
            }
            if (path === '/quality-test-prompts') return json({ prompts: [] })
            if (
              path === '/quality-test-batches/options' ||
              path.endsWith('/quality-test/options')
            ) {
              await body()
              return json({
                models: ['gpt-5.5'],
                reasoning_efforts: ['high', 'low'],
              })
            }
            if (path === '/quality-tests') {
              let list = [...jobs]
                .reverse()
                .filter(
                  (j) =>
                    (!url.searchParams.get('account_id') ||
                      j.account_id ===
                        Number(url.searchParams.get('account_id'))) &&
                    (!url.searchParams.get('channel') ||
                      j.channel === url.searchParams.get('channel')),
                )
              if (url.searchParams.get('latest') === 'true')
                list = list.filter(
                  (j, i) =>
                    list.findIndex(
                      (other) => other.account_id === j.account_id,
                    ) === i,
                )
              const total = list.length,
                page = Number(url.searchParams.get('page') || 1)
              list = list.slice((page - 1) * 20, page * 20)
              return json({
                jobs: list.map(({ output, prompt, ...meta }) => meta),
                active_jobs: [],
                total,
                concurrency_limit: 3,
                facets: {
                  plans: ['pro', 'plus'],
                  models: ['gpt-5.5'],
                  efforts: ['high'],
                  accounts: accounts.map((a) => ({ id: a.id, name: a.name })),
                  presets: [
                    { kind: 'builtin', ref: 'pelican', name: 'Pelican' },
                  ],
                },
              })
            }
            if (path === '/quality-test-batches' && req.method === 'POST') {
              const data = await body()
              submissions.push(data)
              if (batches.has(data.request_id))
                return json(batches.get(data.request_id), 202)
              const ids = data.account_ids.map((id) => {
                const a = accounts.find((a) => a.id === id)
                const job = {
                  ...jobs[0],
                  id: jobs.length + 1,
                  account_id: id,
                  account_name: a.name,
                  status: 'queued',
                  output: '',
                  prompt: data.prompt,
                  batch_id: data.request_id,
                }
                jobs.push(job)
                return job.id
              })
              const batch = {
                id: data.request_id,
                job_ids: ids,
                rejected: [],
                counts: { queued: ids.length },
              }
              batches.set(batch.id, batch)
              return json(batch, 202)
            }
            const batchMatch = path.match(
              /^\/quality-test-batches\/([^/]+)(\/cancel)?$/,
            )
            if (batchMatch) {
              const batch = batches.get(batchMatch[1])
              if (!batch) return json({ error: 'missing' }, 404)
              if (batchMatch[2]) {
                jobs.forEach((j) => {
                  if (j.batch_id === batch.id) j.status = 'stopped'
                })
                batch.counts = { stopped: batch.job_ids.length }
              }
              return json(batch)
            }
            const match = path.match(
              /^\/quality-tests\/(\d+)(\/retry|\/cancel)?$/,
            )
            if (match) {
              let job = jobs.find((j) => j.id === Number(match[1]))
              if (!job) return json({ error: 'missing' }, 404)
              if (match[2] === '/retry') {
                job = {
                  ...job,
                  id: jobs.length + 1,
                  status: 'queued',
                  output: '',
                }
                jobs.push(job)
              }
              if (match[2] === '/cancel') job.status = 'stopped'
              if (!match[2]) {
                active++
                detailRequests++
                peak = Math.max(peak, active)
                await new Promise((r) => setTimeout(r, 60))
                active--
              }
              return json({ job })
            }
            return json({})
          })
        },
      },
    ],
  })
  await server.listen()
  const address = server.httpServer.address()
  return {
    server,
    url: 'http://127.0.0.1:' + address.port,
    submissions,
    get peak() {
      return peak
    },
    get detailRequests() {
      return detailRequests
    },
    complete() {
      for (const batch of batches.values()) {
        jobs.forEach((j) => {
          if (j.batch_id === batch.id && j.status === 'queued') {
            j.status = 'completed'
            j.output = output
          }
        })
        batch.counts = { completed: batch.job_ids.length }
      }
    },
  }
}
