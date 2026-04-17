export default function handler(req, res) {
  if (req.method !== 'POST') return res.status(405).end()

  const { device_code } = req.body

  if (device_code !== 'sim_device_code') {
    return res.status(400).json({ error: 'invalid_request' })
  }

  res.json({ access_token: process.env.SIM_TOKEN || 'tok_simtest123' })
}
