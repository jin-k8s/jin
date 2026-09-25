/** Renders `backtick` spans from plan text as inline code. */
export function Rich({ text }: { text: string }) {
  const parts = text.split(/(`[^`]+`)/g)
  return (
    <>
      {parts.map((p, i) =>
        p.length > 2 && p.startsWith('`') && p.endsWith('`') ? (
          <code key={i} className="rounded bg-panel-2 px-1 py-0.5 font-mono text-[0.9em] text-fg">
            {p.slice(1, -1)}
          </code>
        ) : (
          p
        ),
      )}
    </>
  )
}
