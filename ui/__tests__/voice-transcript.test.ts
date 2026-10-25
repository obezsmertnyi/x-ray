import { describe, expect, it } from "vitest"
import { joinTranscript } from "@/hooks/use-voice-session"

describe("joinTranscript", () => {
  it("puts one space between word chunks", () => {
    expect(joinTranscript("This sets up the", "function")).toBe("This sets up the function")
  })
  it("adds nothing before punctuation", () => {
    expect(joinTranscript("routing for the application", ",")).toBe("routing for the application,")
    expect(joinTranscript("that is it", ".")).toBe("that is it.")
  })
  it("keeps a chunk's own leading space and does not double one", () => {
    expect(joinTranscript("hello", " world")).toBe("hello world")
    expect(joinTranscript("hello ", "world")).toBe("hello world")
  })
  it("starts from the first chunk", () => {
    expect(joinTranscript("", "Hello")).toBe("Hello")
  })
})
