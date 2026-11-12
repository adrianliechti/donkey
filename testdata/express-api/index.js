// `npx express-generator`, or any assistant asked for "a REST API in Express",
// produces roughly this. Note what it does NOT produce: a health route, and
// any use of process.env.PORT.
const express = require('express')
const cors = require('cors')

const app = express()
const port = 3000

app.use(cors())
app.use(express.json())

const todos = [
  { id: 1, title: 'Write the API', done: true },
  { id: 2, title: 'Deploy it somewhere', done: false },
]

app.get('/todos', (req, res) => res.json(todos))

app.post('/todos', (req, res) => {
  const todo = { id: todos.length + 1, title: req.body.title, done: false }
  todos.push(todo)
  res.status(201).json(todo)
})

app.listen(port, () => {
  console.log(`Example app listening on port ${port}`)
})
