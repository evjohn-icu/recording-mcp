import { createRoot } from 'react-dom/client'
import Root from './App'

const container = document.getElementById('root')
if (container) {
  createRoot(container).render(<Root />)
}
