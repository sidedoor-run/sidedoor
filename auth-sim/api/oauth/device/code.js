export default function handler(req, res) {
  if (req.method !== 'POST') return res.status(405).end()

  const base = process.env.VERCEL_URL
    ? `https://${process.env.VERCEL_URL}`
    : 'http://localhost:3000'

  res.json({
    device_code: 'sim_device_code',
    user_code: 'SIDE-DOOR',
    verification_uri: `${base}/activate`,
    expires_in: 900,
    interval: 3,
  })
}
