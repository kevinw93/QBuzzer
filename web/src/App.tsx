import { useRoute } from './router'
import { Home } from './views/Home'
import { ModeratorView } from './views/ModeratorView'
import { PlayerView } from './views/PlayerView'

export default function App() {
  const route = useRoute()
  switch (route.page) {
    case 'home':
      return <Home />
    case 'player':
      return <PlayerView key={route.code} code={route.code} />
    case 'moderator':
      return <ModeratorView key={route.code} code={route.code} />
  }
}
