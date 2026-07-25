import { useEffect, useState } from 'react'

// Written on a laptop, where the API happens to run on port 3001. This line
// is the whole point of the sample: it runs in the visitor's browser, so
// after a deploy "localhost" is *their* machine, not the backend.
const API = 'http://localhost:3001/api/todos'

export function App() {
  const [todos, setTodos] = useState([])
  const [error, setError] = useState('')

  useEffect(() => {
    fetch(API)
      .then((r) => r.json())
      .then(setTodos)
      .catch((e) => setError(String(e)))
  }, [])

  return (
    <main style={{ fontFamily: 'ui-sans-serif, system-ui', maxWidth: '32rem', margin: '3rem auto' }}>
      <h1>Todos</h1>
      {error && (
        <p style={{ color: '#b4543e' }}>
          Could not reach <code>{API}</code>: {error}
        </p>
      )}
      <ul>
        {todos.map((t) => (
          <li key={t.id}>{t.title}</li>
        ))}
      </ul>
    </main>
  )
}
