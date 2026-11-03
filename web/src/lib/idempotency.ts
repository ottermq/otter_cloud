// RetryKey picks the Idempotency-Key for a request that returns a password. A
// request with no answer may still have worked, so the same request retried
// keeps its key and can't happen twice. Any answer ends that: a refused
// request left nothing behind.
export class RetryKey {
  private unanswered: { key: string; body: string } | null = null

  for(body: string): string {
    if (this.unanswered?.body !== body) this.unanswered = { key: crypto.randomUUID(), body }
    return this.unanswered.key
  }

  answered(): void {
    this.unanswered = null
  }
}
