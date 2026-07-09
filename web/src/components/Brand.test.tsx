import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { Brand } from "./Brand";
import { render } from "../test/utils";

describe("Brand", () => {
  it("shows the wordmark", () => {
    render(<Brand />);
    expect(screen.getByText("llmbox")).toBeInTheDocument();
  });

  it("shows the signed-in email when provided", () => {
    render(<Brand email="a@b.c" />);
    expect(screen.getByText("a@b.c")).toBeInTheDocument();
  });
});
