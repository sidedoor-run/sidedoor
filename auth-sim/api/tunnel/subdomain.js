module.exports = function handler(req, res) {
  if (req.method !== 'GET') return res.status(405).end()

  const auth = req.headers.authorization || ''
  const token = auth.replace('Bearer ', '').trim()
  const expected = process.env.SIM_TOKEN || 'tok_simtest123'

  if (token !== expected) {
    return res.status(401).json({ error: 'invalid_token' })
  }

  res.json({ subdomain: process.env.SIM_SUBDOMAIN || 'jonathan' })
}
