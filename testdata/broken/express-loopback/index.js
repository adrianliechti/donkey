// The same mistake as flask-loopback, in the other language people start in.
//
// Express binds every interface when you call listen(port). Pass a host and
// it binds only that one — and '127.0.0.1' inside a container is a network
// with exactly one machine on it. The app logs that it is listening, and it
// is: to itself.
const express = require('express')

const app = express()

app.get('/', (req, res) => res.send('It works'))

app.listen(3000, '127.0.0.1', () => {
  console.log('listening on http://127.0.0.1:3000')
})
