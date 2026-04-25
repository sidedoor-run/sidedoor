module.exports = function handler(req, res) {
  if (req.method !== 'GET') return res.status(405).end()

  res.json({
    version: process.env.DESKTOP_VERSION || '0.0.1',
    required: process.env.DESKTOP_REQUIRED_VERSION || '0.0.1',
    download_url: process.env.DESKTOP_DOWNLOAD_URL || 'https://sidedoor.run',
  })
}
