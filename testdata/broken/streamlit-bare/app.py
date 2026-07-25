# A Streamlit script with nothing to say how it starts.
#
# Railpack sees Python and an app.py and starts it the only way it knows:
# `python app.py`. Streamlit calls that bare mode — every st.* call becomes a
# no-op, a "missing ScriptRunContext" warning is logged for each one, no
# server starts and no port is bound. The container runs. Nothing listens.
#
# It is not a crash and it does not look like one. The fix is a Procfile, or
# any start command that runs this *with* streamlit rather than with python.

import streamlit as st

st.set_page_config(page_title="Quarterly report", page_icon="📈")
st.title("Quarterly report")
st.metric("Revenue", "€581,500", "+12.4%")
st.bar_chart({"revenue": [128_000, 141_500, 139_200, 172_800]})
