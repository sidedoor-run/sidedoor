module.exports = function handler(req, res) {
  if (req.method !== 'GET') return res.status(405).end()

  const auth = req.headers.authorization || ''
  const token = auth.replace('Bearer ', '').trim()
  const expected = process.env.SIM_TOKEN || 'tok_simtest123'

  if (token !== expected) {
    return res.status(401).json({ error: 'invalid_token' })
  }

  const maxTunnels = parseInt(process.env.SIM_MAX_TUNNELS || '1', 10)

  res.json({
    subdomain: process.env.SIM_SUBDOMAIN || 'jonathan',
    domain: process.env.SIM_DOMAIN || 'sidedoor.run',
    max_tunnels: maxTunnels,
  })
}
