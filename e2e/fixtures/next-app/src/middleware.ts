import { NextResponse, type NextRequest } from "next/server";

const SESSION = "sid";

export function middleware(req: NextRequest) {
  if (req.cookies.get(SESSION)) {
    return NextResponse.next();
  }
  return NextResponse.redirect(new URL("/login", req.url));
}

export const config = {
  matcher: ["/dashboard/:path*", "/api/admin/(.*)"],
};
