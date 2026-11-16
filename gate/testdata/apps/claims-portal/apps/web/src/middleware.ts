import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

const SESSION_COOKIE = "cp_session";

// Every page requires a signed-in adjuster. API routes are excluded below so the
// dashboard's own fetches keep working without a cookie round-trip.
export function middleware(req: NextRequest) {
  if (req.cookies.get(SESSION_COOKIE)) {
    return NextResponse.next();
  }
  const login = new URL("/login", req.url);
  login.searchParams.set("next", req.nextUrl.pathname);
  return NextResponse.redirect(login);
}

// Everything except API routes, Next internals and the favicon goes through the
// session check above.
export const config = {
  // One regex, easy to extend when new public paths appear.
  // eslint-disable-next-line @typescript-eslint/naming-convention
  matcher: ["/((?!api|login|_next/static|favicon.ico).*)"],
};
