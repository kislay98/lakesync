import { useMemo, useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { cn, copyText, prettyJson } from '@/lib/utils'

/** Syntax-highlighted JSON (no external deps). */
export function JsonView({ value, className, maxHeight }: { value: unknown; className?: string; maxHeight?: string }) {
  const text = useMemo(() => prettyJson(value), [value])
  const html = useMemo(() => highlight(text), [text])
  const [copied, setCopied] = useState(false)
  return (
    <div className={cn('relative group rounded-md border bg-muted/40', className)}>
      <Button
        variant="ghost"
        size="icon"
        className="absolute right-1 top-1 h-7 w-7 opacity-0 group-hover:opacity-100"
        onClick={async () => {
          if (await copyText(text)) {
            setCopied(true)
            setTimeout(() => setCopied(false), 1200)
          }
        }}
        aria-label="Copy JSON"
      >
        {copied ? <Check className="h-3.5 w-3.5 text-success" /> : <Copy className="h-3.5 w-3.5" />}
      </Button>
      <pre className="overflow-auto p-3 font-mono text-[12px] leading-5 scrollbar-thin" style={{ maxHeight }} dangerouslySetInnerHTML={{ __html: html }} />
    </div>
  )
}

function esc(s: string) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

function highlight(json: string): string {
  const re = /("(\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+-]?\d+)?)/g
  return esc(json).replace(re, (m) => {
    let cls = 'text-info'
    if (/^"/.test(m)) cls = /:$/.test(m) ? 'text-foreground/80 font-medium' : 'text-success'
    else if (/true|false/.test(m)) cls = 'text-warning'
    else if (/null/.test(m)) cls = 'text-muted-foreground'
    return `<span class="${cls}">${m}</span>`
  })
}
