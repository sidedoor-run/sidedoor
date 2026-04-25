module.exports = function handler(req, res) {
  if (req.method !== 'GET') return res.status(405).end()

  res.json({
    version: process.env.SIDEDOOR_VERSION || '0.0.1',
    required: process.env.SIDEDOOR_REQUIRED_VERSION || '0.0.1',
    download_url: process.env.SIDEDOOR_DOWNLOAD_URL || 'https://sidedoor.run',
  })
}
