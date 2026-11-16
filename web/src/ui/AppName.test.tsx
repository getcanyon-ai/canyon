import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { AppName } from "./AppName";

describe("AppName", () => {
  it("says the id beside a name that differs from it, since names repeat and ids never do", () => {
    const { container } = render(<AppName app={{ id: "funny-snake-4k2", name: "claims-app" }} />);
    expect(container.textContent).toBe("claims-app funny-snake-4k2") // a real space: copied text and a screen reader keep the two apart;
    expect(container.querySelector(".font-mono")?.textContent).toBe("funny-snake-4k2");
  });

  it("says the id once when it is the name, as on every app made before names were their own", () => {
    expect(render(<AppName app={{ id: "claims-portal", name: "claims-portal" }} />).container.textContent).toBe("claims-portal");
  });
});
