module.exports = function handler(req, res) {
  if (req.method !== 'POST') return res.status(405).end()

  res.json({ access_token: process.env.SIM_TOKEN || 'tok_simtest123' })
}
