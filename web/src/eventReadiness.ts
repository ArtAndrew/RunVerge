// SPDX-License-Identifier: Elastic-2.0
import {createContext} from 'react'
export const EventReadinessContext=createContext<{paused:boolean;refresh:()=>void}>({paused:false,refresh:()=>undefined})
